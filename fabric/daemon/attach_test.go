package daemon

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// TestCreateChildServesEndpointWhileRunning is the core regression test for
// the reported bug: an agent onboarded while the daemon is running must be
// immediately reachable at its MCP endpoint without a daemon restart.
func TestCreateChildServesEndpointWhileRunning(t *testing.T) {
	srv := stubAcceptAll(t)
	defer srv.Close()

	// Grab a free localhost port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	setHeartbeatEvery(50 * time.Millisecond)
	defer setHeartbeatEvery(30 * time.Second)

	dir := t.TempDir()
	prof := &profile.Profile{
		User: "Eric", Framing: "we ship",
		Root: profile.Root{ID: "root-id", Key: "root-key", InboxChannel: "rootchan"},
	}
	st := &agentstate.State{
		Daemon: agentstate.Daemon{RouterURL: srv.URL, OutboundChannel: "out", MCPAddr: addr},
		Agents: []agentstate.Agent{{ID: "root-id", Key: "root-key", Name: "root", InboxChannel: "rootchan"}},
	}
	d := NewRuntime(Config{
		State:     st,
		StatePath: filepath.Join(dir, "state.json"),
		Hub:       hubclient.NewFake(),
		Control:   control.NewClient(srv.URL),
		Profile:   prof,
		MintKey:   func() string { return "childkey" },
		Domain:    "botbus.ai",
	})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()

	// Wait for the listener to come up.
	deadline := time.After(2 * time.Second)
	for {
		_, derr := (&http.Client{Timeout: 200 * time.Millisecond}).Get("http://" + addr + "/a/doesnotexist")
		if derr == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("MCP listener never came up: %v", derr)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Onboard a new child while the daemon is running.
	child, inst, err := d.CreateChild(ctx, "reviewer", "review")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	// The endpoint must be immediately reachable (not 404).
	req, _ := http.NewRequest(http.MethodPost, inst.MCPEndpoint, strings.NewReader("{}"))
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST to new agent endpoint: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("new agent endpoint returned 404 — route not registered after CreateChild")
	}

	// ReadInbox must not return "unknown agent id".
	_, err = d.ReadInbox(ctx, child.ID, 1)
	if err != nil {
		t.Fatalf("ReadInbox after CreateChild: %v", err)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestLazyAttachFromDisk verifies that an agent onboarded by another process
// (written directly to the state file) is served by the running daemon after
// a single request triggers the disk reload.
func TestLazyAttachFromDisk(t *testing.T) {
	srv := stubAcceptAll(t)
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	setHeartbeatEvery(50 * time.Millisecond)
	defer setHeartbeatEvery(30 * time.Second)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	st := &agentstate.State{
		Daemon: agentstate.Daemon{RouterURL: srv.URL, OutboundChannel: "out", MCPAddr: addr},
		Agents: []agentstate.Agent{{ID: "root-id", Key: "root-key", Name: "root", InboxChannel: "rootchan"}},
	}
	// Write initial state to disk.
	if err := agentstate.Save(statePath, st); err != nil {
		t.Fatal(err)
	}
	d := NewRuntime(Config{
		State:     st,
		StatePath: statePath,
		Hub:       hubclient.NewFake(),
		Control:   control.NewClient(srv.URL),
		MintKey:   func() string { return "k" },
	})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()

	// Wait for listener.
	deadline := time.After(2 * time.Second)
	for {
		_, derr := (&http.Client{Timeout: 200 * time.Millisecond}).Get("http://" + addr + "/a/doesnotexist")
		if derr == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("MCP listener never came up: %v", derr)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Simulate another process onboarding a new agent by writing directly to disk.
	newAgent := agentstate.Agent{
		ID: "new-agent-id", Key: "new-agent-key", Name: "outsider", InboxChannel: "inbox-new",
	}
	disk, err := agentstate.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	disk.Upsert(newAgent)
	if err := agentstate.Save(statePath, disk); err != nil {
		t.Fatal(err)
	}

	// A request to the new agent's endpoint should trigger disk reload and not 404.
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/a/new-agent-key", strings.NewReader("{}"))
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST to lazily-attached agent: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("agent from disk returned 404 — lazy attach not working")
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRemoveDetachesEndpoint verifies that after detach, ReadInbox errors and
// resolveHandler returns nil.
func TestRemoveDetachesEndpoint(t *testing.T) {
	st := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "a1", Key: "k1", Name: "one", InboxChannel: "inbox-a1"},
	}}
	d := NewRuntime(Config{State: st, Hub: hubclient.NewFake()})

	// Confirm the agent is known before detach.
	if _, err := d.ReadInbox(context.Background(), "a1", 1); err != nil {
		t.Fatalf("ReadInbox before detach: %v", err)
	}

	d.detach("a1")

	// After detach, ReadInbox must error with unknown id.
	if _, err := d.ReadInbox(context.Background(), "a1", 1); err == nil {
		t.Fatal("expected error for detached agent id")
	}

	// resolveHandler must return nil (no state entry, no disk).
	if h := d.resolveHandler("k1"); h != nil {
		t.Fatal("expected resolveHandler to return nil after detach")
	}
}

// TestRemoveFromDiskDoesNotResurrect exercises the full Remove path with a
// statePath set: Remove deletes the agent from disk and detaches it, and a
// later request must NOT bring it back via the disk-reload self-heal path.
func TestRemoveFromDiskDoesNotResurrect(t *testing.T) {
	srv := stubAcceptAll(t)
	defer srv.Close()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	st := &agentstate.State{
		Daemon: agentstate.Daemon{RouterURL: srv.URL, OutboundChannel: "out"},
		Agents: []agentstate.Agent{
			{ID: "root-id", Key: "root-key", Name: "root", InboxChannel: "rootchan"},
			{ID: "a1", Key: "k1", Name: "one", InboxChannel: "inbox-a1"},
		},
	}
	if err := agentstate.Save(statePath, st); err != nil {
		t.Fatal(err)
	}
	d := NewRuntime(Config{
		State:     st,
		StatePath: statePath,
		Hub:       hubclient.NewFake(),
		Control:   control.NewClient(srv.URL),
	})

	// Reachable (and cached) before removal.
	if h := d.resolveHandler("k1"); h == nil {
		t.Fatal("agent should be reachable before Remove")
	}

	if _, err := d.Remove(context.Background(), "a1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Gone from disk.
	disk, err := agentstate.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Get("a1"); ok {
		t.Fatal("agent still on disk after Remove")
	}

	// A later request must not resurrect it via disk reload.
	if h := d.resolveHandler("k1"); h != nil {
		t.Fatal("removed agent was resurrected — resolveHandler should return nil after Remove")
	}
	if _, err := d.ReadInbox(context.Background(), "a1", 1); err == nil {
		t.Fatal("expected ReadInbox to error for removed agent")
	}
}
