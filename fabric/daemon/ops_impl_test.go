package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/wire"
)

// TestRosterUsesRootCreds is the brief's required test: root is found via the
// in-memory state agents slice.
func TestRosterUsesRootCreds(t *testing.T) {
	// stubRoster returns a single node iff the request carries root's id+key.
	srv := stubRoster(t, "root-id", "root-key")
	defer srv.Close()
	st := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "root-id", Key: "root-key", Name: "root"},
	}}
	d := NewRuntime(Config{State: st, Hub: hubclient.NewFake(), Control: control.NewClient(srv.URL)})
	nodes, err := d.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "root" {
		t.Fatalf("nodes=%+v", nodes)
	}
}

// TestRootPrefersProfile verifies that a loaded profile takes precedence over
// in-memory state when resolving the root agent.
func TestRootPrefersProfile(t *testing.T) {
	srv := stubRoster(t, "profile-id", "profile-key")
	defer srv.Close()
	// State has a "root" agent but profile should win.
	st := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "state-root-id", Key: "state-root-key", Name: "root"},
	}}
	p := &profile.Profile{
		Root: profile.Root{ID: "profile-id", Key: "profile-key", InboxChannel: "inbox-profile"},
	}
	d := NewRuntime(Config{State: st, Hub: hubclient.NewFake(), Control: control.NewClient(srv.URL), Profile: p})
	nodes, err := d.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster with profile: %v", err)
	}
	// stubRoster only accepts profile-id/profile-key, so a successful response
	// proves the profile was used.
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %+v", nodes)
	}
}

// TestRootFallsBackToDisk verifies the third resolution tier: the local state
// file on disk, when neither profile nor in-memory state has a "root" entry.
func TestRootFallsBackToDisk(t *testing.T) {
	srv := stubRoster(t, "disk-root-id", "disk-root-key")
	defer srv.Close()

	// Write a state file with a root agent.
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	diskState := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "disk-root-id", Key: "disk-root-key", Name: "root", InboxChannel: "i-root"},
	}}
	if err := agentstate.Save(statePath, diskState); err != nil {
		t.Fatalf("save state: %v", err)
	}

	// In-memory state has no "root" entry, so disk is the fallback.
	st := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "other-id", Key: "other-key", Name: "other"},
	}}
	d := NewRuntime(Config{
		State: st, StatePath: statePath,
		Hub: hubclient.NewFake(), Control: control.NewClient(srv.URL),
	})
	nodes, err := d.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster with disk fallback: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %+v", nodes)
	}
}

// TestRootNoRootAnywhere verifies the error returned when no root agent is
// found via any resolution path.
func TestRootNoRootAnywhere(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	// State file exists but contains no root agent.
	if err := agentstate.Save(statePath, &agentstate.State{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	d := NewRuntime(Config{
		State: &agentstate.State{}, StatePath: statePath,
		Hub: hubclient.NewFake(), Control: control.NewClient("http://unused"),
	})
	_, err := d.Roster(context.Background())
	if err == nil {
		t.Fatal("expected error when no root agent exists")
	}
}

// TestRootStateFileReadError verifies the error path when the state file
// cannot be read (a directory in place of the file causes a read error).
func TestRootStateFileReadError(t *testing.T) {
	dir := t.TempDir()
	// Use a path that is a directory (not a readable JSON file) to trigger an
	// error from agentstate.Load inside hostagent.GetByName.
	badPath := filepath.Join(dir, "notafile")
	if err := os.MkdirAll(badPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	d := NewRuntime(Config{
		State: &agentstate.State{}, StatePath: badPath,
		Hub: hubclient.NewFake(), Control: control.NewClient("http://unused"),
	})
	_, err := d.Roster(context.Background())
	if err == nil {
		t.Fatal("expected error from unreadable state path")
	}
}

// TestRosterControlError verifies that a non-200 from the control server is
// propagated as an error (covers the Roster error branch).
func TestRosterControlError(t *testing.T) {
	// A server that always returns 401.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	st := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "root-id", Key: "root-key", Name: "root"},
	}}
	d := NewRuntime(Config{State: st, Hub: hubclient.NewFake(), Control: control.NewClient(srv.URL)})
	_, err := d.Roster(context.Background())
	if err == nil {
		t.Fatal("expected error from failing control server")
	}
}

func TestEnsureRootCreatesThenReuses(t *testing.T) {
	dir := t.TempDir()
	statePath := dir + "/state.json"
	srv := stubAcceptAll(t) // mint + register always 200
	defer srv.Close()
	d := NewRuntime(Config{
		State: &agentstate.State{}, StatePath: statePath,
		Hub: hubclient.NewFake(), Control: control.NewClient(srv.URL),
		MintKey: func() string { return "rootkey" }, Domain: "botbus.ai",
	})
	a1, err := d.EnsureRoot(context.Background())
	if err != nil || a1.Name != "root" {
		t.Fatalf("EnsureRoot #1: %v %+v", err, a1)
	}
	a2, err := d.EnsureRoot(context.Background())
	if err != nil || a2.ID != a1.ID {
		t.Fatalf("EnsureRoot #2 should reuse: %v %+v", err, a2)
	}
}

func stubAcceptAll(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/mint" {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "minted-id"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCreateChildSeedsWelcomeAndBuildsInstructions(t *testing.T) {
	dir := t.TempDir()
	srv := stubAcceptAll(t)
	defer srv.Close()
	fake := hubclient.NewFake()
	prof := &profile.Profile{User: "Eric", Framing: "we ship",
		Root: profile.Root{ID: "root-id", Key: "root-key", InboxChannel: "rootchan"}}
	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "root-id", Key: "root-key", Name: "root"}},
		Daemon: agentstate.Daemon{MCPAddr: "127.0.0.1:8765"}}
	d := NewRuntime(Config{State: st, StatePath: dir + "/state.json", Hub: fake,
		Control: control.NewClient(srv.URL), Profile: prof,
		MintKey: func() string { return "childkey" }, Domain: "botbus.ai"})

	child, inst, err := d.CreateChild(context.Background(), "botbus-cli", "the CLI")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if child.Parent != "root-id" {
		t.Fatalf("child.Parent=%q want root-id", child.Parent)
	}
	if inst.MCPCommand == "" || inst.MCPEndpoint != "http://127.0.0.1:8765/a/childkey" {
		t.Fatalf("instructions=%+v", inst)
	}
	if inst.ChannelURL != "https://"+child.InboxChannel+".botbus.ai/" {
		t.Fatalf("channelURL=%q", inst.ChannelURL)
	}
	// Welcome was published to the child's inbox channel.
	if got := fake.Published(child.InboxChannel); len(got) == 0 {
		t.Fatalf("no welcome seeded to %s", child.InboxChannel)
	}
}

// stubRoster serves GET /v1/agents, returning one "root" node only when the
// request carries the expected X-Agent-Id + Bearer key.
func stubRoster(t *testing.T, wantID, wantKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Id") != wantID || r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]wire.AgentNode{{ID: wantID, Name: "root"}})
	}))
}
