package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/fsnotify/fsnotify"
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

func TestStateWatchFsnotifyAdoptsFasterThanPoll(t *testing.T) {
	// Skip if fsnotify can't initialize in this environment.
	if w, err := fsnotify.NewWatcher(); err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	} else {
		_ = w.Close()
	}
	srv := stubAcceptAll(t)
	defer srv.Close()
	// Poll far away (3s) so adoption within ~1.5s can only come from fsnotify.
	setStatePollEvery(3 * time.Second)
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
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	// Give the watcher a moment to register the directory watch.
	time.Sleep(150 * time.Millisecond)

	rotated := &agentstate.State{Daemon: st.Daemon, Agents: st.Agents,
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 2, Key: k(2), Roster: "roster"}}}
	if err := agentstate.Save(p, rotated); err != nil {
		t.Fatal(err)
	}
	ws := &d.state.Workspaces[0]
	waitFor(t, 1500*time.Millisecond, func() bool { // < 3s poll ⇒ fsnotify drove it
		key, ok := d.currentKey(ws)
		return ok && key == [32]byte(k(2))
	})
}

func TestStateWatchFallsBackToPollWhenFsnotifyUnavailable(t *testing.T) {
	// Force the fsnotify-unavailable path.
	orig := newStateWatcher
	newStateWatcher = func() (*fsnotify.Watcher, error) { return nil, fmt.Errorf("forced unavailable") }
	defer func() { newStateWatcher = orig }()

	srv := stubAcceptAll(t)
	defer srv.Close()
	setStatePollEvery(10 * time.Millisecond)
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
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	rotated := &agentstate.State{Daemon: st.Daemon, Agents: st.Agents,
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 2, Key: k(2), Roster: "roster"}}}
	if err := agentstate.Save(p, rotated); err != nil {
		t.Fatal(err)
	}
	ws := &d.state.Workspaces[0]
	waitFor(t, 2*time.Second, func() bool { // poll-only must still adopt
		key, ok := d.currentKey(ws)
		return ok && key == [32]byte(k(2))
	})
}

