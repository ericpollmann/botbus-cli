package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-proto/wire"
)

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

// In roster mode, esc quits the program (no chat to return from).
func TestRosterModeEscQuits(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	_, cmd := updConsole(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc in roster mode should return a quit command")
	}
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
