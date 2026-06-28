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
