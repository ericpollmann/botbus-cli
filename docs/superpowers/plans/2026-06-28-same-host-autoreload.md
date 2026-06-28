# Same-Host Daemon Auto-Reload Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the long-running daemon adopt external `state.json` changes — written by a one-shot admin command (`workspace key-rotate` / `admit` / `remove`) on the **same host** — live, without a restart and without disrupting any open hub subscription.

**Architecture:** A single daemon-resident goroutine polls `state.json`'s mtime. On change it calls an extended `reloadFromDisk`, which (a) additively merges genuinely-new agents and (b) reconciles **existing** workspaces' key/epoch/anchors/pending **in place** under `d.mu`. Because the inbox opener already re-reads the workspace key per frame (`d.currentKey`) and all loops hold live `*agentstate.Workspace` pointers into `d.state.Workspaces`, updating those struct fields in place is observed immediately by the running loops — no loop is cancelled or re-subscribed. New agents are wired in via the existing idempotent `attach`. The watcher never writes `state.json`.

**Tech Stack:** Go 1.25.x, stdlib only (`os`, `time`, `sync/atomic`, `bytes`, `context`), existing `golang.org/x/crypto` (already a dependency), `github.com/ericpollmann/botbus-proto/hubclient` (test fake).

## Global Constraints

- **No new dependencies.** stdlib + already-present `x/crypto` only.
- **All `d.state` mutation happens under `d.mu`.** The hot key-read path (`currentKey`, `e2eContextFor`, `applyRekey`) already reads/writes `ws.Key`/`ws.Epoch` under `d.mu`; the watcher's writes must too.
- **Mutate existing workspace structs in place; NEVER append to `d.state.Workspaces` while serving.** Running loops (`runRoster`, the inbox `openerFor` closure via `e2eContextFor`) hold `*agentstate.Workspace` pointers into the slice's backing array; appending can reallocate it and orphan those pointers, silently freezing rotation adoption. A workspace present on disk but absent in memory is therefore **skipped** (adopted on next restart), not appended.
- **Monotonic epoch:** never roll a workspace key backward (`disk.Epoch >= mem.Epoch` gate), matching `applyRekey`.
- **The watcher is read-only w.r.t. disk:** it `Load`s and reconciles memory; it never `Save`s. (Avoids an mtime feedback loop and pointless writes.)
- **Connection-preserving:** reconcile MUST NOT cancel/restart any inbox/roster/waiting-room loop. It only updates in-memory values and `attach`es genuinely-new agents (`attach` is idempotent — a no-op for already-running agents).
- **Never log or print key bytes.** `state.json` stays mode `0600` (enforced by `agentstate.Save`).
- **No legacy/compat shims:** `reloadFromDisk`'s return type changes from `bool` to `[]agentstate.Agent`; update its sole caller (`resolveHandler`) in the same commit.
- **Tests:** fake hub + temp `state.json`; no network. The concurrency test runs under `-race`.

---

## File Structure

- **Modify `fabric/daemon/daemon.go`**
  - `reloadFromDisk` — extend to reconcile workspaces; change return `bool → []agentstate.Agent`.
  - `resolveHandler` — update the single caller (`if d.reloadFromDisk() {` → `if len(d.reloadFromDisk()) > 0 {`).
  - `RunOn` — start the watcher goroutine via `g.Go`.
- **Create `fabric/daemon/statewatch.go`**
  - `reconcileWorkspacesLocked` + helpers (`indexOfWorkspaceLocked`, `anchorsEqual`, `pendingEqual`, `cloneAnchors`, `clonePending`).
  - `runStateWatch`, `statMTime`, `statePollEveryNs` + `setStatePollEvery`/`getStatePollEvery`.
- **Create `fabric/daemon/reload_reconcile_test.go`** — Task 1 unit tests.
- **Create `fabric/daemon/statewatch_test.go`** — Task 2 serving integration tests (+ `countingHub` + `waitFor` helpers).
- **Create `fabric/daemon/statewatch_race_test.go`** — Task 3 `-race` stress test.
- **Modify `README.md`** — Task 4 docs.

---

### Task 1: Extend `reloadFromDisk` to reconcile existing workspaces (key/epoch/anchors/pending) and return new agents

**Files:**
- Create: `fabric/daemon/statewatch.go` (reconcile half only this task)
- Modify: `fabric/daemon/daemon.go` (`reloadFromDisk` body + return type; `resolveHandler` caller)
- Test: `fabric/daemon/reload_reconcile_test.go`

**Interfaces:**
- Consumes: `agentstate.State{Agents []Agent; Workspaces []Workspace}`, `agentstate.Workspace{RootID; Epoch uint32; Key []byte; Anchors []AnchorRef; Pending []PendingJoin}`, `agentstate.AnchorRef{ID; SignPub; EncPub []byte}`, `agentstate.PendingJoin{ReqID; Name; ParentIntent string; SignPub; EncPub []byte}`, `agentstate.Load(path)`, `d.agentByKeyLocked(key)`, `d.trust.anchors.set(id, ed25519.PublicKey)` / `d.trust.anchors.lookup(id) (ed25519.PublicKey, bool)`.
- Produces: `func (d *Daemon) reloadFromDisk() []agentstate.Agent` (newly-added agents; nil if none), `func (d *Daemon) reconcileWorkspacesLocked(disk []agentstate.Workspace)` (caller holds `d.mu`).

- [ ] **Step 1: Write the failing tests** in `fabric/daemon/reload_reconcile_test.go`

```go
package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

// newReconcileDaemon builds a non-serving daemon whose in-memory state is `mem`
// and whose statePath points at a fresh (initially absent) temp file. Tests
// Save the desired on-disk ("post one-shot CLI") state to that path, then call
// d.reloadFromDisk() directly.
func newReconcileDaemon(t *testing.T, mem *agentstate.State) (*Daemon, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state.json")
	d := NewRuntime(Config{State: mem, StatePath: p, Hub: hubclient.NewFake()})
	return d, p
}

func k(n byte) []byte { return bytes.Repeat([]byte{n}, 32) }

func TestReloadAdoptsRotatedKey(t *testing.T) {
	mem := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1)}}}
	d, p := newReconcileDaemon(t, mem)
	disk := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 2, Key: k(2)}}}
	if err := agentstate.Save(p, disk); err != nil {
		t.Fatal(err)
	}
	d.reloadFromDisk()
	if d.state.Workspaces[0].Epoch != 2 || !bytes.Equal(d.state.Workspaces[0].Key, k(2)) {
		t.Fatalf("key not adopted: epoch=%d", d.state.Workspaces[0].Epoch)
	}
}

func TestReloadMonotonicEpochNoRollback(t *testing.T) {
	mem := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 5, Key: k(1)}}}
	d, p := newReconcileDaemon(t, mem)
	disk := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 3, Key: k(9)}}}
	if err := agentstate.Save(p, disk); err != nil {
		t.Fatal(err)
	}
	d.reloadFromDisk()
	if d.state.Workspaces[0].Epoch != 5 || !bytes.Equal(d.state.Workspaces[0].Key, k(1)) {
		t.Fatal("rolled back below in-memory epoch")
	}
}

func TestReloadAdoptsAnchorsAndTrust(t *testing.T) {
	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	encPub, _, _ := box.GenerateKey(rand.Reader)
	mem := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1)}}}
	d, p := newReconcileDaemon(t, mem)
	disk := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1),
		Anchors: []agentstate.AnchorRef{{ID: "bob", SignPub: signPub, EncPub: encPub[:]}}}}}
	if err := agentstate.Save(p, disk); err != nil {
		t.Fatal(err)
	}
	d.reloadFromDisk()
	if len(d.state.Workspaces[0].Anchors) != 1 || d.state.Workspaces[0].Anchors[0].ID != "bob" {
		t.Fatal("anchor not adopted into workspace list")
	}
	pub, ok := d.trust.anchors.lookup("bob")
	if !ok || !bytes.Equal(pub, signPub) {
		t.Fatal("anchor not added to trust graph")
	}
}

func TestReloadRemovesAnchorOnEviction(t *testing.T) {
	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	encPub, _, _ := box.GenerateKey(rand.Reader)
	mem := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1),
		Anchors: []agentstate.AnchorRef{{ID: "bob", SignPub: signPub, EncPub: encPub[:]}}}}}
	d, p := newReconcileDaemon(t, mem)
	// Disk after `workspace remove bob`: anchor gone + key rotated to epoch 2.
	disk := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 2, Key: k(2)}}}
	if err := agentstate.Save(p, disk); err != nil {
		t.Fatal(err)
	}
	d.reloadFromDisk()
	if len(d.state.Workspaces[0].Anchors) != 0 {
		t.Fatal("evicted anchor not removed from list")
	}
	if d.state.Workspaces[0].Epoch != 2 || !bytes.Equal(d.state.Workspaces[0].Key, k(2)) {
		t.Fatal("rotation not adopted on eviction")
	}
}

func TestReloadReconcilesPending(t *testing.T) {
	mem := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1)}}}
	d, p := newReconcileDaemon(t, mem)
	disk := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1),
		Pending: []agentstate.PendingJoin{{ReqID: "req1", Name: "carol", SignPub: []byte{1}, EncPub: []byte{2}}}}}}
	if err := agentstate.Save(p, disk); err != nil {
		t.Fatal(err)
	}
	d.reloadFromDisk()
	if len(d.state.Workspaces[0].Pending) != 1 || d.state.Workspaces[0].Pending[0].ReqID != "req1" {
		t.Fatal("pending not reconciled")
	}
}

func TestReloadMergesNewAgentsReturnsThem(t *testing.T) {
	mem := &agentstate.State{Agents: []agentstate.Agent{{ID: "a", Key: "ka", InboxChannel: "ia"}}}
	d, p := newReconcileDaemon(t, mem)
	disk := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "a", Key: "ka", InboxChannel: "ia"},
		{ID: "b", Key: "kb", InboxChannel: "ib"},
	}}
	if err := agentstate.Save(p, disk); err != nil {
		t.Fatal(err)
	}
	added := d.reloadFromDisk()
	if len(added) != 1 || added[0].ID != "b" {
		t.Fatalf("expected new agent b returned, got %v", added)
	}
	if _, ok := d.state.AgentByID("b"); !ok {
		t.Fatal("agent b not merged into memory")
	}
}

func TestReloadSkipsUnknownWorkspace(t *testing.T) {
	mem := &agentstate.State{Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1)}}}
	d, p := newReconcileDaemon(t, mem)
	disk := &agentstate.State{Workspaces: []agentstate.Workspace{
		{RootID: "root", E2E: true, Epoch: 1, Key: k(1)},
		{RootID: "other", E2E: true, Epoch: 1, Key: k(7)},
	}}
	if err := agentstate.Save(p, disk); err != nil {
		t.Fatal(err)
	}
	d.reloadFromDisk()
	if len(d.state.Workspaces) != 1 {
		t.Fatalf("unknown workspace must be skipped (not appended); got %d", len(d.state.Workspaces))
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd /Users/pollmann/Documents/hack/botbus-cli/.claude/worktrees/autoreload && go test ./fabric/daemon/ -run 'TestReload' -v`
Expected: compile error / FAIL (`reloadFromDisk` returns `bool`, `reconcileWorkspacesLocked` undefined).

- [ ] **Step 3: Add the reconcile half of `fabric/daemon/statewatch.go`**

```go
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
```

- [ ] **Step 4: Rewrite `reloadFromDisk` in `fabric/daemon/daemon.go`** (replace the existing function at lines ~224–248)

```go
// reloadFromDisk reconciles in-memory state with state.json: it additively
// merges agents present on disk but not in memory (returning them so the caller
// can attach them), and reconciles EXISTING workspaces' key/epoch/anchors/
// pending in place (see reconcileWorkspacesLocked). Never drops in-memory agents
// or workspaces and never writes to disk. No-op if statePath is empty. Returns
// the agents newly added to memory (nil if none).
func (d *Daemon) reloadFromDisk() []agentstate.Agent {
	if d.statePath == "" {
		return nil
	}
	st, err := agentstate.Load(d.statePath)
	if err != nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var added []agentstate.Agent
	for _, a := range st.Agents {
		// Dedup by capability key (the routing identity). attach later dedups
		// loop-startup by agent ID.
		if _, known := d.agentByKeyLocked(a.Key); !known {
			d.state.Agents = append(d.state.Agents, a)
			added = append(added, a)
		}
	}
	d.reconcileWorkspacesLocked(st.Workspaces)
	return added
}
```

- [ ] **Step 5: Update the sole caller in `resolveHandler`** (`fabric/daemon/daemon.go` ~line 262)

Change:
```go
	if !ok {
		if d.reloadFromDisk() {
			d.mu.Lock()
			a, ok = d.agentByKeyLocked(key)
			d.mu.Unlock()
		}
	}
```
to:
```go
	if !ok {
		if len(d.reloadFromDisk()) > 0 {
			d.mu.Lock()
			a, ok = d.agentByKeyLocked(key)
			d.mu.Unlock()
		}
	}
```

- [ ] **Step 6: Run the tests + the existing daemon suite**

Run: `cd /Users/pollmann/Documents/hack/botbus-cli/.claude/worktrees/autoreload && go test ./fabric/daemon/ -run 'TestReload' -v && go test ./fabric/daemon/`
Expected: all new `TestReload*` PASS; the full `fabric/daemon` package still PASS (no regression from the `reloadFromDisk` signature change).

- [ ] **Step 7: Commit**

```bash
git add fabric/daemon/statewatch.go fabric/daemon/daemon.go fabric/daemon/reload_reconcile_test.go
git commit -m "feat(daemon): reloadFromDisk reconciles workspace key/epoch/anchors/pending"
```

---

### Task 2: Add the `runStateWatch` mtime-poll loop and wire it into `RunOn`

**Files:**
- Modify: `fabric/daemon/statewatch.go` (add watcher half)
- Modify: `fabric/daemon/daemon.go` (`RunOn` — start the watcher)
- Test: `fabric/daemon/statewatch_test.go`

**Interfaces:**
- Consumes: `d.statePath`, `d.reloadFromDisk()` (Task 1), `d.attach(a)`, `d.currentKey(ws)`, `agentstate.Save`, `hubclient.Fake` (`.Subscribe`, `.Inject`).
- Produces: `func (d *Daemon) runStateWatch(ctx context.Context)`, `func statMTime(path string) time.Time`, `func setStatePollEvery(d time.Duration)` / `func getStatePollEvery() time.Duration`, package var `statePollEveryNs int64`.

- [ ] **Step 1: Write the failing tests** in `fabric/daemon/statewatch_test.go`

```go
package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// countingHub wraps a Fake and counts Subscribe calls per channel, so a test can
// prove a live subscription was NOT torn down across a state reload.
type countingHub struct {
	*hubclient.Fake
	mu   sync.Mutex
	subs map[string]int
}

func newCountingHub() *countingHub {
	return &countingHub{Fake: hubclient.NewFake(), subs: map[string]int{}}
}

func (c *countingHub) Subscribe(ctx context.Context, channel, resume string) (<-chan hubclient.Frame, error) {
	c.mu.Lock()
	c.subs[channel]++
	c.mu.Unlock()
	return c.Fake.Subscribe(ctx, channel, resume)
}

func (c *countingHub) count(channel string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subs[channel]
}

func waitFor(t *testing.T, max time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", max)
}

// freeAddr binds then releases a localhost port, returning its address for the
// daemon's MCP listener (mirrors fabric/daemon/attach_test.go).
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestStateWatchAdoptsRotationWithoutReconnect(t *testing.T) {
	srv := stubAcceptAll(t)
	defer srv.Close()
	setStatePollEvery(10 * time.Millisecond)
	defer setStatePollEvery(2 * time.Second)

	p := filepath.Join(t.TempDir(), "state.json")
	_, signSeed, _ := ed25519.GenerateKey(rand.Reader)
	st := &agentstate.State{
		Daemon:     agentstate.Daemon{RouterURL: srv.URL, MCPAddr: freeAddr(t)},
		Agents:     []agentstate.Agent{{ID: "root", Key: "rootkey", InboxChannel: "root-inbox", SignSeed: signSeed.Seed()}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1), Roster: "roster"}},
	}
	if err := agentstate.Save(p, st); err != nil {
		t.Fatal(err)
	}

	hub := newCountingHub()
	d := NewRuntime(Config{State: st, StatePath: p, Hub: hub, Control: control.NewClient(srv.URL), Domain: "botbus.ai"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool { return hub.count("root-inbox") >= 1 })
	subsBefore := hub.count("root-inbox")

	// Simulate a one-shot `workspace key-rotate`: rewrite state.json epoch2/key2.
	rotated := &agentstate.State{
		Daemon:     st.Daemon,
		Agents:     st.Agents,
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 2, Key: k(2), Roster: "roster"}},
	}
	if err := agentstate.Save(p, rotated); err != nil {
		t.Fatal(err)
	}

	ws := &d.state.Workspaces[0]
	waitFor(t, 2*time.Second, func() bool {
		key, ok := d.currentKey(ws)
		return ok && key == [32]byte(k(2))
	})

	if got := hub.count("root-inbox"); got != subsBefore {
		t.Fatalf("inbox re-subscribed (%d → %d): reload disrupted the live connection", subsBefore, got)
	}
}

func TestStateWatchAttachesNewAgentWithoutDisruptingExisting(t *testing.T) {
	srv := stubAcceptAll(t)
	defer srv.Close()
	setStatePollEvery(10 * time.Millisecond)
	defer setStatePollEvery(2 * time.Second)

	p := filepath.Join(t.TempDir(), "state.json")
	st := &agentstate.State{
		Daemon: agentstate.Daemon{RouterURL: srv.URL, MCPAddr: freeAddr(t)},
		Agents: []agentstate.Agent{{ID: "root", Key: "rootkey", InboxChannel: "root-inbox"}},
	}
	if err := agentstate.Save(p, st); err != nil {
		t.Fatal(err)
	}

	hub := newCountingHub()
	d := NewRuntime(Config{State: st, StatePath: p, Hub: hub, Control: control.NewClient(srv.URL), Domain: "botbus.ai"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool { return hub.count("root-inbox") >= 1 })
	rootSubs := hub.count("root-inbox")

	// Simulate a one-shot command adding a new local agent.
	withChild := &agentstate.State{
		Daemon: st.Daemon,
		Agents: []agentstate.Agent{
			{ID: "root", Key: "rootkey", InboxChannel: "root-inbox"},
			{ID: "child", Key: "childkey", InboxChannel: "child-inbox"},
		},
	}
	if err := agentstate.Save(p, withChild); err != nil {
		t.Fatal(err)
	}

	// New agent's loop comes up...
	waitFor(t, 2*time.Second, func() bool { return hub.count("child-inbox") >= 1 })
	// ...without disturbing the existing agent's subscription.
	if got := hub.count("root-inbox"); got != rootSubs {
		t.Fatalf("existing inbox re-subscribed (%d → %d) when attaching a new agent", rootSubs, got)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd /Users/pollmann/Documents/hack/botbus-cli/.claude/worktrees/autoreload && go test ./fabric/daemon/ -run 'TestStateWatch' -v`
Expected: compile error (`setStatePollEvery` undefined) → FAIL.

- [ ] **Step 3: Append the watcher half to `fabric/daemon/statewatch.go`**

Add these imports to the file's import block: `"context"`, `"os"`, `"sync/atomic"`, `"time"`. Then add:

```go
// statePollEveryNs controls how often the daemon polls state.json for external
// modifications. Atomic so tests can shorten it. Default 2s: a one-shot admin
// command is adopted within ~2s, and the per-tick cost is a single os.Stat plus
// — only when the mtime changed — a small-file Load, negligible beside the SSE
// I/O the daemon already does per frame.
var statePollEveryNs int64 = int64(2 * time.Second)

func setStatePollEvery(d time.Duration) { atomic.StoreInt64(&statePollEveryNs, int64(d)) }
func getStatePollEvery() time.Duration  { return time.Duration(atomic.LoadInt64(&statePollEveryNs)) }

// runStateWatch polls statePath and, when its mtime changes (an external
// one-shot CLI wrote it), reconciles the in-memory state via reloadFromDisk:
// existing workspaces' key/epoch/anchors/pending are adopted in place and
// genuinely-new agents are attached — WITHOUT cancelling or restarting any
// inbox/roster/waiting-room loop, so live hub subscriptions are preserved. The
// watcher never writes state.json. No-op when statePath is empty.
func (d *Daemon) runStateWatch(ctx context.Context) {
	if d.statePath == "" {
		return
	}
	last := statMTime(d.statePath)
	t := time.NewTicker(getStatePollEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m := statMTime(d.statePath)
			if m.Equal(last) {
				continue
			}
			last = m
			for _, a := range d.reloadFromDisk() {
				d.attach(a) // idempotent; starts the new agent's loops, no effect on existing agents
			}
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
```

- [ ] **Step 4: Wire the watcher into `RunOn`** (`fabric/daemon/daemon.go`)

Immediately AFTER the `for i := range d.state.Workspaces { ... }` loop and BEFORE `srv := &http.Server{Handler: d.mux()}`, add:

```go
	// Watch state.json for same-host one-shot CLI writes (key-rotate/admit/
	// remove) and adopt them live without disrupting any loop.
	g.Go(func() error { d.runStateWatch(gctx); return nil })
```

- [ ] **Step 5: Run the new tests**

Run: `cd /Users/pollmann/Documents/hack/botbus-cli/.claude/worktrees/autoreload && go test ./fabric/daemon/ -run 'TestStateWatch' -v`
Expected: both PASS.

- [ ] **Step 6: Commit**

```bash
git add fabric/daemon/statewatch.go fabric/daemon/daemon.go fabric/daemon/statewatch_test.go
git commit -m "feat(daemon): live state.json mtime-poll auto-reload (no connection disruption)"
```

---

### Task 3: `-race` stress test for the watcher under concurrent traffic

**Files:**
- Test: `fabric/daemon/statewatch_race_test.go`

**Interfaces:**
- Consumes: `d.Run`, `d.currentKey`, `d.e2eContextFor`, `d.reloadFromDisk`, `agentstate.Save`, `setStatePollEvery`, the `countingHub`/`waitFor`/`freeAddr`/`k` helpers from Tasks 1–2.

- [ ] **Step 1: Write the stress test** in `fabric/daemon/statewatch_race_test.go`

```go
package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
)

// TestStateWatchNoRaceUnderTraffic runs the watcher at a 1ms poll while an
// external writer rewrites state.json with monotonically increasing epochs and
// reader goroutines hammer the locked key-read paths. Run with -race: any
// unsynchronised access to d.state / ws.Key fails the test.
func TestStateWatchNoRaceUnderTraffic(t *testing.T) {
	srv := stubAcceptAll(t)
	defer srv.Close()
	setStatePollEvery(time.Millisecond)
	defer setStatePollEvery(2 * time.Second)

	p := filepath.Join(t.TempDir(), "state.json")
	st := &agentstate.State{
		Daemon:     agentstate.Daemon{RouterURL: srv.URL, MCPAddr: freeAddr(t)},
		Agents:     []agentstate.Agent{{ID: "root", Key: "rootkey", InboxChannel: "root-inbox"}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1), Roster: "roster"}},
	}
	if err := agentstate.Save(p, st); err != nil {
		t.Fatal(err)
	}

	d := NewRuntime(Config{State: st, StatePath: p, Hub: newCountingHub(), Control: control.NewClient(srv.URL), Domain: "botbus.ai"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Run(ctx) }()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// External writer: rewrite state.json with rising epochs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		epoch := uint32(2)
		for {
			select {
			case <-stop:
				return
			default:
			}
			next := &agentstate.State{
				Daemon:     st.Daemon,
				Agents:     st.Agents,
				Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: epoch, Key: k(byte(epoch)), Roster: "roster"}},
			}
			_ = agentstate.Save(p, next)
			epoch++
			time.Sleep(time.Millisecond)
		}
	}()

	// Readers: hammer the locked key paths concurrently with the watcher.
	ws := &d.state.Workspaces[0]
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = d.currentKey(ws)
				_, _, _ = d.e2eContextFor("root")
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	cancel()
}
```

- [ ] **Step 2: Run under the race detector**

Run: `cd /Users/pollmann/Documents/hack/botbus-cli/.claude/worktrees/autoreload && go test ./fabric/daemon/ -run 'TestStateWatchNoRaceUnderTraffic' -race -count=1 -v`
Expected: PASS with no `DATA RACE` report. If a race is reported, fix the lock discipline (all `d.state` / `ws.Key` access under `d.mu`) before proceeding — do NOT weaken the test.

- [ ] **Step 3: Run the full daemon package under -race** (regression guard)

Run: `cd /Users/pollmann/Documents/hack/botbus-cli/.claude/worktrees/autoreload && go test ./fabric/daemon/ -race -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add fabric/daemon/statewatch_race_test.go
git commit -m "test(daemon): -race stress test for state-watch reconcile under traffic"
```

---

### Task 4: Document the live auto-reload; remove the stale "restart admin host" limitation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Locate the cross-host limitation note.** Search `README.md` for the paragraph (added by the e2e-runnable feature) describing the limitation that, after a one-shot admin command on the admin's own host, that host's daemon must be restarted (remote hosts adopt via the roster). It mentions "restart" the admin/host daemon and notes same-host auto-reload as a follow-up.

- [ ] **Step 2: Replace that limitation note** with the following (adapt surrounding heading/wording to match the file's style):

```markdown
#### Same-host live reload

A one-shot admin command (`botbus workspace key-rotate` / `admit` / `remove`)
writes the change to `state.json`. The running daemon on that same host adopts it
**live**: a background watcher polls `state.json` (~2s) and, on change, reconciles
the in-memory workspace key/epoch/anchors/pending in place and attaches any new
local agent — **without restarting or re-subscribing any hub connection**. The
inbox opener re-reads the workspace key per frame, so a rotation takes effect on
the next inbound frame with no dropped subscription. Remote hosts adopt the same
change via the encrypted roster channel as before.

**Known limitation:** the live reload covers *existing* workspaces. A brand-new
workspace created while the daemon is running (e.g. `workspace join` on a host
that had none) is adopted only on the next daemon restart — appending a workspace
at runtime would invalidate the pointers held by running loops, so it is
deliberately deferred to restart.
```

- [ ] **Step 3: Verify the file still renders / no broken references** (visual scan; this is a docs-only change).

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document same-host live auto-reload; drop stale restart limitation"
```

---

## Self-Review (controller, before execution)

- **Spec coverage:** key-rotate (key/epoch adopt — T1/T2), remove (anchor drop + rotate — T1), admit (anchor add + trust — T1), pending (T1), new local agent attach (T2), no-reconnect proof (T2 subscribe-count), race-safety (T3), docs (T4). ✓
- **Connection-preserving** is asserted directly (subscribe count unchanged across reload) — Eric's explicit constraint. ✓
- **Scope safety:** new workspaces are skipped (T1 `TestReloadSkipsUnknownWorkspace`) and documented (T4), avoiding the slice-realloc pointer-orphan hazard. ✓
- **Type consistency:** `reloadFromDisk() []agentstate.Agent` used identically in `resolveHandler` (Task 1) and `runStateWatch` (Task 2). ✓
