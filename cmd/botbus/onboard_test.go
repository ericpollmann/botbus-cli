package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
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
func (s *stubOnboardOps) EnsureRoot(context.Context) (agentstate.Agent, error) {
	return agentstate.Agent{}, nil
}
func (s *stubOnboardOps) Addr() string { return "127.0.0.1:8765" }

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
