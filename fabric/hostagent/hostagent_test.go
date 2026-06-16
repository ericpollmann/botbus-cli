package hostagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// stubControl accepts any Bearer-authenticated register/heartbeat. The real
// auth + persistence is exercised in the private router's tests; here we only
// need the client round-trip to succeed.
func stubControl(t *testing.T) *control.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/agents/{id}/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return control.NewClient(srv.URL)
}

func TestCreateMintsRegistersAndPersists(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := Deps{
		Hub:       hubclient.NewFake(),
		Control:   stubControl(t),
		StatePath: statePath,
		MintKey:   func() string { return "key-fixed" },
	}
	a, err := Create(ctx, deps, CreateOpts{ID: "myth-compiler", Focus: "packages/compile", Mode: "session"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.InboxChannel == "" {
		t.Fatal("inbox channel not minted")
	}
	if a.Key != "key-fixed" {
		t.Fatalf("key = %q, want key-fixed", a.Key)
	}

	s, _ := agentstate.Load(statePath)
	if got, ok := s.Get("myth-compiler"); !ok || got.InboxChannel != a.InboxChannel || got.Focus != "packages/compile" {
		t.Fatalf("agent not persisted correctly: %+v ok=%v", got, ok)
	}
}

func TestCreateRejectsDuplicateID(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := Deps{
		Hub:       hubclient.NewFake(),
		Control:   stubControl(t),
		StatePath: statePath,
		MintKey:   func() string { return "key-fixed" },
	}
	if _, err := Create(ctx, deps, CreateOpts{ID: "dup"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := Create(ctx, deps, CreateOpts{ID: "dup"}); err == nil {
		t.Fatal("expected duplicate-id error")
	}
}

func TestCreateRequiresID(t *testing.T) {
	deps := Deps{
		Hub:       hubclient.NewFake(),
		Control:   stubControl(t),
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		MintKey:   func() string { return "k" },
	}
	if _, err := Create(context.Background(), deps, CreateOpts{ID: ""}); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestListReturnsLocalAgents(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "a", Focus: "one", Mode: "session"},
		{ID: "b", Focus: "two", Mode: "spawn"},
	}}
	if err := agentstate.Save(statePath, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := List(statePath)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("List = %+v", got)
	}
}

func TestRemoveDeletesLocalAgent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := &agentstate.State{Agents: []agentstate.Agent{{ID: "a"}, {ID: "b"}}}
	if err := agentstate.Save(statePath, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Remove(statePath, "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	reloaded, _ := agentstate.Load(statePath)
	if _, ok := reloaded.Get("a"); ok {
		t.Fatal("agent a should be gone")
	}
	if _, ok := reloaded.Get("b"); !ok {
		t.Fatal("agent b should remain")
	}
	if err := Remove(statePath, "missing"); err == nil {
		t.Fatal("removing unknown agent should error")
	}
}
