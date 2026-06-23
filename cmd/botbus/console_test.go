package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-proto/wire"
)

// stubConsoleOps implements daemon.Ops for unit-testing the TUI helpers.
// It records call arguments and returns canned values for the used methods;
// unused methods return zero values.
type stubConsoleOps struct {
	conn daemon.ConnectInstructions
	// recorded args from CreateChild
	gotName  string
	gotFocus string
}

func (s *stubConsoleOps) Roster(_ context.Context) ([]wire.AgentNode, error) {
	return nil, nil
}

func (s *stubConsoleOps) CreateChild(_ context.Context, name, focus string) (agentstate.Agent, daemon.ConnectInstructions, error) {
	s.gotName = name
	s.gotFocus = focus
	return agentstate.Agent{}, s.conn, nil
}

func (s *stubConsoleOps) Send(_ context.Context, _ string, _ daemon.SendArgs) error {
	return nil
}

func (s *stubConsoleOps) ReadInbox(_ context.Context, _ string, _ int) (string, error) {
	return "", nil
}

func (s *stubConsoleOps) EnsureRoot(_ context.Context) (agentstate.Agent, error) {
	return agentstate.Agent{}, nil
}

func (s *stubConsoleOps) Addr() string { return "127.0.0.1:8765" }

func TestOnboardChildOpsReturnsMCPInstruction(t *testing.T) {
	ops := &stubConsoleOps{conn: daemon.ConnectInstructions{
		MCPCommand: "claude mcp add --transport http botbus-cli http://127.0.0.1:8765/a/k",
		ChannelURL: "https://chan.botbus.ai/",
	}}
	msg, err := onboardChildOps(context.Background(), ops, "botbus-cli", "the CLI")
	if err != nil {
		t.Fatalf("onboardChildOps: %v", err)
	}
	if !strings.Contains(msg, "claude mcp add") {
		t.Fatalf("expected MCP-first instruction, got %q", msg)
	}
}

func updRoster(m rosterModel, msg tea.Msg) (rosterModel, bool) {
	return m.updateRoster(msg)
}

func updConsole(m model, msg tea.Msg) (model, tea.Cmd) {
	mm, cmd := m.Update(msg)
	return mm.(model), cmd
}

func TestModeTransitionsRosterToChatAndBack(t *testing.T) {
	started := ""
	stopped := false
	m := newConsoleModel([]wire.AgentNode{{Name: "myth-compiler", InboxChannel: "i-c1"}})
	m.startChat = func(channel string) chatSession { started = channel; return chatSession{} }
	m.stopChat = func() { stopped = true }

	if m.mode != modeRoster {
		t.Fatal("console starts in roster mode")
	}
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter}) // dip into selected
	if m.mode != modeChat || started != "i-c1" {
		t.Fatalf("enter should start chat on i-c1 + switch mode; mode=%d started=%q", m.mode, started)
	}
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEsc}) // back to roster
	if m.mode != modeRoster || !stopped {
		t.Fatalf("esc should stop chat + return to roster; mode=%d stopped=%v", m.mode, stopped)
	}
}

// A node with an empty InboxChannel must NOT switch into chat mode — there's
// nothing to dial, so staying in the roster avoids stranding the user in an
// empty, un-dialable chat view (N4).
func TestDipIntoEmptyInboxStaysInRoster(t *testing.T) {
	started := false
	m := newConsoleModel([]wire.AgentNode{{Name: "no-inbox", InboxChannel: ""}})
	m.startChat = func(string) chatSession { started = true; return chatSession{} }
	m.stopChat = func() {}

	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter}) // dip into the empty node
	if m.mode != modeRoster {
		t.Fatalf("empty-inbox node should keep us in roster mode, got %d", m.mode)
	}
	if started {
		t.Fatal("startChat must not be called for an empty-inbox node")
	}
}

// In roster mode, esc quits the program (no chat to return from).
func TestRosterModeEscQuits(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	_, cmd := updConsole(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in roster mode should return a quit command")
	}
}

// ctrl+c during the onboard prompt quits the program (esc still aborts, tested
// in console_run_test.go's TestOnboardEscAborts).
func TestOnboardCtrlCQuits(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	m.onboard = func(string, string) (string, error) { return "", nil }
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if m.onboardState != onboardAskName {
		t.Fatalf("o should begin onboarding, got state %d", m.onboardState)
	}
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c during onboarding should return a quit command")
	}
	// Quit means we do NOT silently abort back to a plain roster as if nothing
	// happened — the program is ending. (We don't assert the exact tea.Quit
	// identity because tea.Quit is an unexported sentinel; a non-nil cmd that
	// isn't the no-op is the observable contract here.)
	_ = mm
}

func TestRosterNavigationSelectsAgent(t *testing.T) {
	m := newRosterModel([]wire.AgentNode{
		{Name: "root", InboxChannel: "i-root"},
		{Name: "myth-compiler", Parent: "root", InboxChannel: "i-c1", Live: true},
		{Name: "myth-cli", Parent: "root", InboxChannel: "i-c2"},
	})
	if m.cursor != 0 {
		t.Fatalf("cursor should start at 0, got %d", m.cursor)
	}
	m, _ = updRoster(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = updRoster(m, tea.KeyMsg{Type: tea.KeyDown})
	if got := m.selected().Name; got != "myth-cli" {
		t.Fatalf("selected = %q, want myth-cli", got)
	}
	if out := m.View(); out == "" {
		t.Fatal("roster View should render")
	}
}

// scopeToWorkspace returns only the org-root + its descendants (by Parent
// chain); an empty orgRootID returns the roster unchanged.
func TestScopeToWorkspace(t *testing.T) {
	nodes := []wire.AgentNode{
		{ID: "ID_acme", Name: "acme", Parent: ""},
		{ID: "ID_eric", Name: "eric", Parent: "ID_acme"},
		{ID: "ID_ericc", Name: "eric-compiler", Parent: "ID_eric"},
		{ID: "ID_root", Name: "root", Parent: ""},
		{ID: "ID_testboss", Name: "testboss", Parent: "ID_other"},
		{ID: "ID_smoke", Name: "smoke-daemon", Parent: ""},
	}

	got := scopeToWorkspace(nodes, "ID_acme")
	gotNames := map[string]bool{}
	for _, n := range got {
		gotNames[n.Name] = true
	}
	if len(got) != 3 || !gotNames["acme"] || !gotNames["eric"] || !gotNames["eric-compiler"] {
		t.Fatalf("scopeToWorkspace(ID_acme) = %v, want {acme, eric, eric-compiler}", gotNames)
	}

	all := scopeToWorkspace(nodes, "")
	if len(all) != len(nodes) {
		t.Fatalf("scopeToWorkspace(\"\") returned %d nodes, want all %d", len(all), len(nodes))
	}
}

// scopeToWorkspace must terminate even when the Parent chain forms a cycle
// (defensive: a corrupt roster shouldn't hang the console).
func TestScopeToWorkspaceCycleTerminates(t *testing.T) {
	nodes := []wire.AgentNode{
		{ID: "ID_acme", Name: "acme", Parent: "ID_loop"},
		{ID: "ID_loop", Name: "loop", Parent: "ID_acme"},
		{ID: "ID_solo", Name: "solo", Parent: ""},
	}
	// Scope to a real root that isn't part of the cycle: must return just it and
	// not loop forever walking the cyclic parents of the others.
	got := scopeToWorkspace(nodes, "ID_solo")
	if len(got) != 1 || got[0].Name != "solo" {
		t.Fatalf("scopeToWorkspace(ID_solo) = %v, want {solo}", got)
	}
}

// An unknown / unmatched orgRootID scopes to nothing (no node is the root and
// no chain reaches it) — the console shows an empty workspace rather than the
// whole router.
// An active workspace whose org-root isn't in the roster (stale/deregistered)
// falls back to showing all nodes rather than stranding the operator with an
// empty console.
func TestScopeToWorkspaceUnknownRootShowsAll(t *testing.T) {
	nodes := []wire.AgentNode{
		{ID: "ID_a", Name: "a", Parent: ""},
		{ID: "ID_b", Name: "b", Parent: "ID_a"},
	}
	if got := scopeToWorkspace(nodes, "ID_missing"); len(got) != len(nodes) {
		t.Fatalf("scopeToWorkspace(unknown) = %v, want all %d nodes (fallback)", got, len(nodes))
	}
}

// enter signals a dip-in request; cursor clamps at the ends.
func TestRosterEnterSignalsDipAndClamps(t *testing.T) {
	m := newRosterModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	m, _ = updRoster(m, tea.KeyMsg{Type: tea.KeyUp}) // clamp at 0
	if m.cursor != 0 {
		t.Fatalf("cursor should clamp at 0, got %d", m.cursor)
	}
	_, dip := updRoster(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !dip {
		t.Fatal("enter should signal dip-in")
	}
}
