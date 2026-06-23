package hostagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/wire"
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

// stubControlDeregister returns a control client whose DELETE handler records
// the path id + Authorization it received and replies with the given status.
func stubControlDeregister(t *testing.T, status int, gotID, gotAuth *string) *control.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		*gotID = r.PathValue("id")
		*gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(status)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return control.NewClient(srv.URL)
}

// Remove deregisters the agent from the router (with its bound key) AND deletes
// it from local state.
func TestRemoveDeregistersAndDeletesLocal(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "minted-a", Key: "key-a", Name: "rftest"},
		{ID: "minted-b", Key: "key-b"},
	}}
	if err := agentstate.Save(statePath, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotID, gotAuth string
	deps := Deps{Control: stubControlDeregister(t, http.StatusNoContent, &gotID, &gotAuth), StatePath: statePath}

	routerErr, err := Remove(context.Background(), deps, "minted-a")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if routerErr != nil {
		t.Fatalf("routerErr = %v, want nil", routerErr)
	}
	if gotID != "minted-a" || gotAuth != "Bearer key-a" {
		t.Fatalf("router got id=%q auth=%q, want minted-a / Bearer key-a", gotID, gotAuth)
	}
	reloaded, _ := agentstate.Load(statePath)
	if _, ok := reloaded.Get("minted-a"); ok {
		t.Fatal("agent minted-a should be gone locally")
	}
	if _, ok := reloaded.Get("minted-b"); !ok {
		t.Fatal("agent minted-b should remain")
	}
}

// The router call is best-effort: a router failure still removes local state and
// is surfaced separately (so the host stops managing the agent regardless).
func TestRemoveStillDeletesLocalWhenRouterFails(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := &agentstate.State{Agents: []agentstate.Agent{{ID: "minted-a", Key: "key-a"}}}
	if err := agentstate.Save(statePath, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotID, gotAuth string
	deps := Deps{Control: stubControlDeregister(t, http.StatusInternalServerError, &gotID, &gotAuth), StatePath: statePath}

	routerErr, err := Remove(context.Background(), deps, "minted-a")
	if err != nil {
		t.Fatalf("Remove (local) should succeed even when the router fails: %v", err)
	}
	if routerErr == nil {
		t.Fatal("routerErr should be non-nil when the router rejects deregister")
	}
	reloaded, _ := agentstate.Load(statePath)
	if _, ok := reloaded.Get("minted-a"); ok {
		t.Fatal("local agent must be removed even when the router call fails")
	}
}

// An unknown local id errors and never calls the router (no key to present).
func TestRemoveUnknownLocalAgentErrors(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := agentstate.Save(statePath, &agentstate.State{Agents: []agentstate.Agent{{ID: "x"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotID, gotAuth string
	deps := Deps{Control: stubControlDeregister(t, http.StatusNoContent, &gotID, &gotAuth), StatePath: statePath}

	routerErr, err := Remove(context.Background(), deps, "missing")
	if err == nil {
		t.Fatal("removing an unknown local agent should error")
	}
	if routerErr != nil {
		t.Fatalf("routerErr should be nil for an unknown agent, got %v", routerErr)
	}
	if gotID != "" {
		t.Fatalf("router must not be called for an unknown agent, got id=%q", gotID)
	}
}

// With no Control configured, Remove is local-only (best-effort router skipped).
func TestRemoveNilControlSkipsRouter(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := agentstate.Save(statePath, &agentstate.State{Agents: []agentstate.Agent{{ID: "x", Key: "k"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	routerErr, err := Remove(context.Background(), Deps{StatePath: statePath}, "x")
	if err != nil {
		t.Fatalf("Remove with nil Control: %v", err)
	}
	if routerErr != nil {
		t.Fatalf("routerErr should be nil with nil Control, got %v", routerErr)
	}
	reloaded, _ := agentstate.Load(statePath)
	if _, ok := reloaded.Get("x"); ok {
		t.Fatal("agent should be removed locally with nil Control")
	}
}

// A corrupt state file makes Load fail; Remove surfaces it as a fatal error.
func TestRemoveLoadErrorSurfaces(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if _, err := Remove(context.Background(), Deps{StatePath: statePath}, "x"); err == nil {
		t.Fatal("Remove should surface a load-state error on a corrupt state file")
	}
}

// A save failure (here: the atomic-write temp path is occupied by a directory)
// is surfaced as a fatal error even though Load and the router both succeeded.
func TestRemoveSaveErrorSurfaces(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := agentstate.Save(statePath, &agentstate.State{Agents: []agentstate.Agent{{ID: "x", Key: "k"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Mkdir(statePath+".tmp", 0o700); err != nil {
		t.Fatalf("mkdir tmp blocker: %v", err)
	}
	if _, err := Remove(context.Background(), Deps{StatePath: statePath}, "x"); err == nil {
		t.Fatal("Remove should surface a save-state error")
	}
}

// TestRemoveLastAgentClearsState verifies that removing the final managed agent
// is allowed to empty the state file — Remove must thread agentstate.AllowEmpty
// past the empty-clobber guard, or this legitimate case would be refused.
func TestRemoveLastAgentClearsState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := &agentstate.State{Agents: []agentstate.Agent{{ID: "only", Key: "k", InboxChannel: "i"}}}
	if err := agentstate.Save(statePath, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Remove(context.Background(), Deps{StatePath: statePath}, "only"); err != nil {
		t.Fatalf("removing the last agent should succeed: %v", err)
	}
	reloaded, err := agentstate.Load(statePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(reloaded.Agents) != 0 {
		t.Fatalf("state should be empty after removing the last agent, got %+v", reloaded.Agents)
	}
}

// stubControlRegister returns a control client whose PUT handler records the
// path id, Authorization header, and decoded AgentSpec body of the last
// register it received, so a test can assert Update re-registered the new spec.
func stubControlRegister(t *testing.T, gotID, gotAuth *string, gotSpec *wire.AgentSpec) *control.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		*gotID = r.PathValue("id")
		*gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(gotSpec)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return control.NewClient(srv.URL)
}

// Update applies the non-nil fields to the local agent, persists them, and
// re-registers the new spec with the router. Identity (ID/Key/InboxChannel) is
// never changed.
func TestUpdateAppliesFieldsPersistsAndReRegisters(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := &agentstate.State{Agents: []agentstate.Agent{{
		ID: "minted-a", Key: "key-a", Name: "myth-sdk", InboxChannel: "inbox-a",
		Focus: "old focus", Interest: "old interest", Mode: "session",
	}}}
	if err := agentstate.Save(statePath, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotID, gotAuth string
	var gotSpec wire.AgentSpec
	deps := Deps{Control: stubControlRegister(t, &gotID, &gotAuth, &gotSpec), StatePath: statePath}

	newFocus, newInterest := "release freeze — prioritize SDK", "new interest"
	got, err := Update(context.Background(), deps, "myth-sdk", UpdateFields{Focus: &newFocus, Interest: &newInterest})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Returned agent reflects the changes; identity untouched.
	if got.Focus != newFocus || got.Interest != newInterest {
		t.Fatalf("returned agent not updated: %+v", got)
	}
	if got.ID != "minted-a" || got.Key != "key-a" || got.InboxChannel != "inbox-a" || got.Name != "myth-sdk" {
		t.Fatalf("identity must be unchanged: %+v", got)
	}

	// Persisted to local state.
	reloaded, _ := agentstate.Load(statePath)
	persisted, ok := reloaded.Get("minted-a")
	if !ok || persisted.Focus != newFocus || persisted.Interest != newInterest {
		t.Fatalf("changes not persisted: %+v ok=%v", persisted, ok)
	}
	if persisted.InboxChannel != "inbox-a" || persisted.Name != "myth-sdk" {
		t.Fatalf("persisted identity changed: %+v", persisted)
	}

	// Re-registered the NEW spec with the bound key.
	if gotID != "minted-a" || gotAuth != "Bearer key-a" {
		t.Fatalf("router got id=%q auth=%q, want minted-a / Bearer key-a", gotID, gotAuth)
	}
	if gotSpec.Focus != newFocus || gotSpec.Interest != newInterest {
		t.Fatalf("re-registered spec not updated: %+v", gotSpec)
	}
	if gotSpec.InboxChannel != "inbox-a" || gotSpec.Name != "myth-sdk" {
		t.Fatalf("re-registered spec identity changed: %+v", gotSpec)
	}
}

// A nil field leaves the value unchanged; a non-nil empty string clears it.
func TestUpdateNilLeavesFieldEmptyClears(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	s := &agentstate.State{Agents: []agentstate.Agent{{
		ID: "minted-a", Key: "key-a", Name: "n", InboxChannel: "i",
		Focus: "keep me", Interest: "clear me", Parent: "p", Mode: "session", ModelTier: "opus",
	}}}
	if err := agentstate.Save(statePath, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotID, gotAuth string
	var gotSpec wire.AgentSpec
	deps := Deps{Control: stubControlRegister(t, &gotID, &gotAuth, &gotSpec), StatePath: statePath}

	// Focus omitted (nil) → unchanged; Interest set to "" → cleared.
	empty := ""
	got, err := Update(context.Background(), deps, "n", UpdateFields{Interest: &empty})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Focus != "keep me" {
		t.Fatalf("nil Focus should be left unchanged, got %q", got.Focus)
	}
	if got.Interest != "" {
		t.Fatalf("empty-string Interest should clear the field, got %q", got.Interest)
	}
	if got.Parent != "p" || got.ModelTier != "opus" || got.Mode != "session" {
		t.Fatalf("other nil fields should be unchanged: %+v", got)
	}

	reloaded, _ := agentstate.Load(statePath)
	persisted, _ := reloaded.Get("minted-a")
	if persisted.Focus != "keep me" || persisted.Interest != "" {
		t.Fatalf("nil/clear semantics not persisted: %+v", persisted)
	}
}

// Updating an unknown name errors.
func TestUpdateUnknownNameErrors(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := agentstate.Save(statePath, &agentstate.State{Agents: []agentstate.Agent{{ID: "x", Name: "present"}}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var gotID, gotAuth string
	var gotSpec wire.AgentSpec
	deps := Deps{Control: stubControlRegister(t, &gotID, &gotAuth, &gotSpec), StatePath: statePath}

	focus := "x"
	if _, err := Update(context.Background(), deps, "missing", UpdateFields{Focus: &focus}); err == nil {
		t.Fatal("updating an unknown local agent should error")
	}
	if gotID != "" {
		t.Fatalf("router must not be called for an unknown agent, got id=%q", gotID)
	}
}
