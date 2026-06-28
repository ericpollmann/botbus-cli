package daemon

import (
	"bytes"
	"crypto/ed25519"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
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
