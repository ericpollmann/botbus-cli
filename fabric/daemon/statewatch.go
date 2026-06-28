package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/fsnotify/fsnotify"
)

// reconcileWorkspacesLocked updates in-memory workspaces to match `disk`,
// mutating EXISTING workspace structs in place so live loops (which hold
// *agentstate.Workspace pointers into d.state.Workspaces) observe the change
// without a restart. Caller MUST hold d.mu.
//
// Scope: only workspaces already present in memory are reconciled. A workspace
// present on disk but absent in memory is SKIPPED — appending could reallocate
// d.state.Workspaces and orphan the pointers held by running loops, silently
// freezing rotation adoption. New workspaces are adopted on the next restart.
//
// Updated fields: Key/Epoch (monotonic — never rolled backward), Anchors and
// Pending (the admin host's on-disk records are the source of truth). Newly
// trusted anchors are added to the trust graph (additive; eviction is enforced
// by key rotation — the evicted anchor cannot follow the new epoch — and stale
// trust entries are GC'd on the next restart's hydrate).
func (d *Daemon) reconcileWorkspacesLocked(disk []agentstate.Workspace) {
	for i := range disk {
		dw := &disk[i]
		mi := indexOfWorkspaceLocked(d.state.Workspaces, dw.RootID)
		if mi < 0 {
			continue // new workspace on disk — out of scope (see doc above)
		}
		mw := &d.state.Workspaces[mi]
		// Key/epoch: monotonic adoption.
		if len(dw.Key) == 32 && dw.Epoch >= mw.Epoch &&
			(mw.Epoch != dw.Epoch || !bytes.Equal(mw.Key, dw.Key)) {
			mw.Key = append([]byte(nil), dw.Key...)
			mw.Epoch = dw.Epoch
		}
		// Anchors: disk is the source of truth on the admin host.
		if !anchorsEqual(mw.Anchors, dw.Anchors) {
			mw.Anchors = cloneAnchors(dw.Anchors)
			for _, ar := range mw.Anchors {
				if len(ar.SignPub) == ed25519.PublicKeySize {
					d.trust.anchors.set(ar.ID, ed25519.PublicKey(ar.SignPub))
				}
			}
		}
		// Pending: disk is the source of truth on the admin host.
		if !pendingEqual(mw.Pending, dw.Pending) {
			mw.Pending = clonePending(dw.Pending)
		}
	}
}

func indexOfWorkspaceLocked(ws []agentstate.Workspace, rootID string) int {
	for i := range ws {
		if ws[i].RootID == rootID {
			return i
		}
	}
	return -1
}

func anchorsEqual(a, b []agentstate.AnchorRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID ||
			!bytes.Equal(a[i].SignPub, b[i].SignPub) ||
			!bytes.Equal(a[i].EncPub, b[i].EncPub) {
			return false
		}
	}
	return true
}

func pendingEqual(a, b []agentstate.PendingJoin) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ReqID != b[i].ReqID {
			return false
		}
	}
	return true
}

func cloneAnchors(in []agentstate.AnchorRef) []agentstate.AnchorRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentstate.AnchorRef, len(in))
	for i, ar := range in {
		out[i] = agentstate.AnchorRef{
			ID:      ar.ID,
			SignPub: append([]byte(nil), ar.SignPub...),
			EncPub:  append([]byte(nil), ar.EncPub...),
		}
	}
	return out
}

func clonePending(in []agentstate.PendingJoin) []agentstate.PendingJoin {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentstate.PendingJoin, len(in))
	for i, p := range in {
		out[i] = agentstate.PendingJoin{
			ReqID: p.ReqID, Name: p.Name, ParentIntent: p.ParentIntent,
			SignPub: append([]byte(nil), p.SignPub...),
			EncPub:  append([]byte(nil), p.EncPub...),
		}
	}
	return out
}

// statePollEveryNs controls how often the daemon polls state.json for external
// modifications. Atomic so tests can shorten it. Default 2s: a one-shot admin
// command is adopted within ~2s, and the per-tick cost is a single os.Stat plus
// — only when the mtime changed — a small-file Load, negligible beside the SSE
// I/O the daemon already does per frame.
var statePollEveryNs int64 = int64(2 * time.Second)

func setStatePollEvery(d time.Duration) { atomic.StoreInt64(&statePollEveryNs, int64(d)) }
func getStatePollEvery() time.Duration  { return time.Duration(atomic.LoadInt64(&statePollEveryNs)) }

// newStateWatcher constructs the fsnotify watcher; overridable in tests to
// force the fsnotify-unavailable fallback (poll-only) path.
var newStateWatcher = func() (*fsnotify.Watcher, error) { return fsnotify.NewWatcher() }

// runStateWatch reconciles in-memory state with state.json whenever an external
// one-shot CLI writes it. It wakes on two sources: an fsnotify event (low
// latency, best-effort) and a periodic mtime poll (always on — the safety net
// for platforms/filesystems where fsnotify is unavailable or silently drops
// events, and the sole trigger if the watcher fails to start). Either source
// runs the same idempotent reconcile, gated by mtime so a duplicate wake is a
// cheap no-op. The watcher never writes state.json. No-op when statePath is
// empty.
func (d *Daemon) runStateWatch(ctx context.Context) {
	if d.statePath == "" {
		return
	}
	last := statMTime(d.statePath)
	reconcile := func() {
		m := statMTime(d.statePath)
		if m.Equal(last) {
			return
		}
		last = m
		for _, a := range d.reloadFromDisk() {
			d.attach(a) // idempotent; starts new agents' loops, no effect on existing
		}
	}

	// Poll ticker — always on (safety net + fallback).
	t := time.NewTicker(getStatePollEvery())
	defer t.Stop()

	// fsnotify trigger — best-effort. Watch the DIRECTORY (not the file) so the
	// temp+rename agentstate.Save performs doesn't orphan the watch. On any setup
	// error, proceed poll-only.
	var events chan fsnotify.Event
	var errs chan error
	if w, err := newStateWatcher(); err != nil {
		log.Printf("daemon: state watch fsnotify unavailable, polling only: %v", err)
	} else {
		dir := filepath.Dir(d.statePath)
		if dir == "" {
			dir = "."
		}
		if err := w.Add(dir); err != nil {
			log.Printf("daemon: state watch cannot watch %s, polling only: %v", dir, err)
			_ = w.Close()
		} else {
			defer w.Close()
			events = w.Events
			errs = w.Errors
		}
	}

	base := filepath.Base(d.statePath)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile()
		case ev, ok := <-events:
			if !ok {
				events = nil // watcher closed; rely on the poll
				continue
			}
			// The dir also emits events for the .tmp/.bak siblings; react only to
			// our state file. The mtime gate in reconcile() debounces multiple
			// events from a single Save.
			if filepath.Base(ev.Name) == base {
				reconcile()
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			log.Printf("daemon: state watch error: %v", err)
		}
	}
}

// statMTime returns path's modification time, or the zero time if it cannot be
// stat'd (a transient error is treated as "unchanged" so it never triggers a
// spurious reconcile).
func statMTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
