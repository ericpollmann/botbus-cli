package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/wire"
)

func TestSeedSampleTaskPostsStartedFrame(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := seedSampleTask(context.Background(), srv.URL, "mythwork"); err != nil {
		t.Fatalf("seedSampleTask: %v", err)
	}
	if !strings.HasPrefix(gotBody, "mythwork: ") {
		t.Fatalf("frame should be sender-prefixed, got %q", gotBody)
	}
	for _, want := range []string{`"v":1`, `"type":"task.started"`, `"task":"onboarding"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("frame missing %q, got %q", want, gotBody)
		}
	}
}

func TestEnsureWorkspaceRootCreatesAndPersists(t *testing.T) {
	d := fakeDeps(t) // from workspace_test.go: fakes + temp state path
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	root, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("ensureWorkspaceRoot: %v", err)
	}
	if root.Name != "mythwork" || root.Parent != "" {
		t.Fatalf("org-root should be named mythwork with no parent, got %+v", root)
	}

	p, err := profile.Load(profilePath)
	if err != nil {
		t.Fatalf("profile.Load: %v", err)
	}
	if p.User != "eric" || p.Root.ID != root.ID || p.Root.InboxChannel != root.InboxChannel || p.Root.Key != root.Key {
		t.Fatalf("profile not persisted to the org-root: %+v", p)
	}
}

// ensureWorkspaceRoot must mint a workspace source channel, bind it at the router
// (POST /v1/sources with the root's id), persist it on profile.Root.Source, and
// point the daemon's OutboundChannel at it so every agent's `send` publishes to
// the workspace source — the wiring that activates firewalled routing.
func TestEnsureWorkspaceRootBindsSourceAndSetsOutbound(t *testing.T) {
	var boundID, boundChannel string
	var sourceCalls int
	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mint", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("minted-%d", n.Add(1))})
	})
	mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/sources", func(w http.ResponseWriter, r *http.Request) {
		sourceCalls++
		boundID = r.Header.Get("X-Agent-Id")
		var body struct {
			Channel string `json:"channel"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		boundChannel = body.Channel
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := hostagent.Deps{
		Hub:       hubclient.NewFake(),
		Control:   control.NewClient(srv.URL),
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		MintKey:   func() string { return "key-fixed" },
	}
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	root, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("ensureWorkspaceRoot: %v", err)
	}

	p, _ := profile.Load(profilePath)
	if p.Root.Source == "" {
		t.Fatal("profile.Root.Source should be set to the workspace source channel")
	}
	if boundChannel != p.Root.Source {
		t.Fatalf("router bound channel %q != persisted source %q", boundChannel, p.Root.Source)
	}
	if boundID != root.ID {
		t.Fatalf("source bound to id %q, want the root id %q", boundID, root.ID)
	}
	st, _ := agentstate.Load(d.StatePath)
	if st.Daemon.OutboundChannel != p.Root.Source {
		t.Fatalf("Daemon.OutboundChannel = %q, want the workspace source %q", st.Daemon.OutboundChannel, p.Root.Source)
	}

	// Re-onboard the same root: reuse the same source, no fresh mint/rebind churn
	// beyond the idempotent re-bind.
	if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	p2, _ := profile.Load(profilePath)
	if p2.Root.Source != p.Root.Source {
		t.Fatalf("re-run changed the source channel: %q -> %q", p.Root.Source, p2.Root.Source)
	}
}

// A failed source bind must leave NO persisted source and an empty outbound
// channel: AddSource runs before profile/state persistence, so an error aborts
// before anything is written. Locks the "bind before persist" ordering invariant
// against a future refactor that reorders it.
func TestEnsureWorkspaceRootSourceBindFailureLeavesNoState(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var n atomic.Int64
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/mint", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("minted-%d", n.Add(1))})
			})
			mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			mux.HandleFunc("POST /v1/sources", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", status) })
			srv := httptest.NewServer(mux)
			defer srv.Close()

			d := hostagent.Deps{
				Hub:       hubclient.NewFake(),
				Control:   control.NewClient(srv.URL),
				StatePath: filepath.Join(t.TempDir(), "state.json"),
				MintKey:   func() string { return "key-fixed" },
			}
			profilePath := filepath.Join(t.TempDir(), "profile.json")

			if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err == nil {
				t.Fatal("a failed source bind should surface an error")
			}
			p, _ := profile.Load(profilePath)
			if p.Root.Source != "" {
				t.Fatalf("no source should be persisted after a failed bind, got %q", p.Root.Source)
			}
			st, _ := agentstate.Load(d.StatePath)
			if st.Daemon.OutboundChannel != "" {
				t.Fatalf("OutboundChannel must stay empty after a failed bind, got %q", st.Daemon.OutboundChannel)
			}
		})
	}
}

// A profile whose root predates F4b (Root set but Source empty) must self-heal on
// the next onboard: the source == "" branch mints, binds, and persists a fresh
// source. Covers pre-F4b profiles upgrading, and a prior run that died after
// saving the root but before binding the source.
func TestEnsureWorkspaceRootHealsEmptySource(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	first, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Simulate a pre-F4b / partial profile: same root id, no source.
	p, _ := profile.Load(profilePath)
	p.Root.Source = ""
	if err := profile.Save(profilePath, p); err != nil {
		t.Fatalf("save cleared profile: %v", err)
	}

	if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err != nil {
		t.Fatalf("heal run: %v", err)
	}
	healed, _ := profile.Load(profilePath)
	if healed.Root.ID != first.ID {
		t.Fatalf("heal must keep the same root, got %q want %q", healed.Root.ID, first.ID)
	}
	if healed.Root.Source == "" {
		t.Fatal("empty source should have been re-minted and persisted on the next onboard")
	}
	st, _ := agentstate.Load(d.StatePath)
	if st.Daemon.OutboundChannel != healed.Root.Source {
		t.Fatalf("OutboundChannel = %q, want the healed source %q", st.Daemon.OutboundChannel, healed.Root.Source)
	}
}

func TestEnsureWorkspaceRootReusesOnRerun(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	first, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-run should reuse the same org-root id, got %q then %q", first.ID, second.ID)
	}
	agents, _ := hostagent.List(d.StatePath)
	if len(agents) != 1 {
		t.Fatalf("re-run should not mint a second root, got %d agents", len(agents))
	}
}

func TestEnsureWorkspaceRootPreservesFraming(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	// Pre-seed a profile with an existing directive/framing.
	if err := profile.Save(profilePath, &profile.Profile{User: "eric", Framing: "ship fast"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err != nil {
		t.Fatalf("ensureWorkspaceRoot: %v", err)
	}
	p, err := profile.Load(profilePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Framing != "ship fast" {
		t.Fatalf("existing Framing should be preserved, got %q", p.Framing)
	}
}

func TestEnsureWorkspaceRootSurfacesCorruptProfile(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(profilePath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed corrupt profile: %v", err)
	}
	if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err == nil {
		t.Fatal("a corrupt profile should surface an error, not be overwritten")
	}
	got, _ := os.ReadFile(profilePath)
	if string(got) != "{not valid json" {
		t.Fatalf("corrupt profile must not be overwritten, got %q", got)
	}
}

// stubOnboardOps satisfies daemon.Ops for the orchestration test: CreateChild
// returns canned local-MCP connect instructions; the rest are no-ops.
type stubOnboardOps struct{ createdName, createdFocus string }

func (s *stubOnboardOps) Roster(context.Context) ([]wire.AgentNode, error) { return nil, nil }
func (s *stubOnboardOps) CreateChild(_ context.Context, name, focus string) (agentstate.Agent, daemon.ConnectInstructions, error) {
	s.createdName, s.createdFocus = name, focus
	return agentstate.Agent{Name: name}, daemon.ConnectInstructions{
		MCPCommand: "claude mcp add --transport http " + name + " http://127.0.0.1:8765/a/ck",
		ChannelURL: "https://child.botbus.ai/",
	}, nil
}
func (s *stubOnboardOps) Send(context.Context, string, daemon.SendArgs) error    { return nil }
func (s *stubOnboardOps) ReadInbox(context.Context, string, int) (string, error) { return "", nil }
func (s *stubOnboardOps) Addr() string                                           { return "127.0.0.1:8765" }

func (s *stubOnboardOps) Remove(_ context.Context, _ string) (error, error) { return nil, nil }

func TestOnboardStepsHappyPath(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	stub := &stubOnboardOps{}
	rebuild := func(*profile.Profile) daemon.Ops { return stub }

	// name / workspace / directive / teammate / (finish invites) / agent / focus
	in := strings.NewReader("eric\nmythwork\nShip v1\nethan\n\nmyth-compiler\nthe compiler\n")
	var out bytes.Buffer

	boardURL, err := onboardSteps(in, &out, d, profilePath, rebuild)
	if err != nil {
		t.Fatalf("onboardSteps: %v", err)
	}
	if !strings.Contains(boardURL, ".botbus.ai") {
		t.Fatalf("boardURL should be the workspace channel, got %q", boardURL)
	}

	s := out.String()
	if !strings.Contains(s, "claude mcp add") {
		t.Fatalf("step 2 should print the local connect command, got:\n%s", s)
	}
	if !strings.Contains(s, "ethan") || !strings.Contains(s, "?user=ethan") {
		t.Fatalf("step 4 should print ethan's join URL, got:\n%s", s)
	}
	if stub.createdName != "myth-compiler" || stub.createdFocus != "the compiler" {
		t.Fatalf("step 5 should create the agent via CreateChild, got name=%q focus=%q", stub.createdName, stub.createdFocus)
	}

	p, _ := profile.Load(profilePath)
	if p.User != "eric" || p.Framing != "Ship v1" || p.Root.ID == "" {
		t.Fatalf("profile not set up by the wizard: %+v", p)
	}
}

func TestOnboardStepsRequiresName(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	rebuild := func(*profile.Profile) daemon.Ops { return &stubOnboardOps{} }
	if _, err := onboardSteps(strings.NewReader("\n"), &bytes.Buffer{}, d, profilePath, rebuild); err == nil {
		t.Fatal("an empty name should error")
	}
}
