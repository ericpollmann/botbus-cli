package hostagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// stubControl mints a fresh id per call and accepts any Bearer-authenticated
// register/heartbeat. The real auth + HMAC id validation is exercised in the
// private router's tests; here we only need the client round-trips to succeed.
func stubControl(t *testing.T) *control.Client {
	t.Helper()
	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mint", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("minted-%d", n.Add(1))})
	})
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
	a, err := Create(ctx, deps, CreateOpts{Name: "myth-compiler", Focus: "packages/compile", Mode: "session"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.InboxChannel == "" {
		t.Fatal("inbox channel not minted")
	}
	if a.Key != "key-fixed" {
		t.Fatalf("key = %q, want key-fixed", a.Key)
	}
	// id is the router-minted opaque token, NOT the human name.
	if a.ID == "" || a.ID == "myth-compiler" {
		t.Fatalf("id should be a minted opaque token, got %q", a.ID)
	}
	if a.Name != "myth-compiler" {
		t.Fatalf("name = %q, want myth-compiler", a.Name)
	}

	s, _ := agentstate.Load(statePath)
	if got, ok := s.Get(a.ID); !ok || got.Name != "myth-compiler" || got.InboxChannel != a.InboxChannel || got.Focus != "packages/compile" {
		t.Fatalf("agent not persisted correctly: %+v ok=%v", got, ok)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := Deps{
		Hub:       hubclient.NewFake(),
		Control:   stubControl(t),
		StatePath: statePath,
		MintKey:   func() string { return "key-fixed" },
	}
	if _, err := Create(ctx, deps, CreateOpts{Name: "dup"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := Create(ctx, deps, CreateOpts{Name: "dup"}); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestCreateRequiresName(t *testing.T) {
	deps := Deps{
		Hub:       hubclient.NewFake(),
		Control:   stubControl(t),
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		MintKey:   func() string { return "k" },
	}
	if _, err := Create(context.Background(), deps, CreateOpts{Name: ""}); err == nil {
		t.Fatal("expected error for empty name")
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

// flakyControl mints fresh ids and fails the FIRST register with 500, then
// succeeds. registerCount records how many registers were attempted so a test
// can assert no second mint/register storm.
func flakyControl(t *testing.T, registers *int) *control.Client {
	t.Helper()
	var n atomic.Int64
	var regN atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mint", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("minted-%d", n.Add(1))})
	})
	mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, _ *http.Request) {
		if regN.Add(1) == 1 {
			http.Error(w, "router down", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		*registers = int(regN.Load())
	})
	return control.NewClient(srv.URL)
}

// EnsureRoot reuses an existing local root rather than minting a second one. If
// the first attempt's register fails (router down) the local root is persisted;
// a later EnsureRoot must reuse it (re-register), NOT create a duplicate.
func TestEnsureRootReusesExistingLocalRootAfterRegisterFailure(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.json")
	var registers int
	deps := Deps{
		Hub:       hubclient.NewFake(),
		Control:   flakyControl(t, &registers),
		StatePath: statePath,
		MintKey:   func() string { return "key-fixed" },
	}

	// First attempt: register fails → error, but the local root was persisted.
	if _, err := EnsureRoot(ctx, deps); err == nil {
		t.Fatal("first EnsureRoot should fail (register down)")
	}
	agents, err := List(statePath)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "root" {
		t.Fatalf("local root should persist after a failed register: %+v", agents)
	}
	firstID := agents[0].ID

	// Second attempt: must REUSE the existing local root (no "already exists"
	// error, no second mint) and succeed once the router is back.
	root, err := EnsureRoot(ctx, deps)
	if err != nil {
		t.Fatalf("second EnsureRoot should reuse the local root and succeed: %v", err)
	}
	if root.ID != firstID {
		t.Fatalf("reused root id = %q, want the persisted %q", root.ID, firstID)
	}
	agents, _ = List(statePath)
	if len(agents) != 1 {
		t.Fatalf("EnsureRoot must not create a second root: %+v", agents)
	}
}

// With no existing local root, EnsureRoot mints + registers a fresh one.
func TestEnsureRootCreatesWhenAbsent(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.json")
	deps := Deps{
		Hub:       hubclient.NewFake(),
		Control:   stubControl(t),
		StatePath: statePath,
		MintKey:   func() string { return "key-fixed" },
	}
	root, err := EnsureRoot(ctx, deps)
	if err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	if root.Name != "root" || root.ID == "" || root.InboxChannel == "" {
		t.Fatalf("fresh root not populated: %+v", root)
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
