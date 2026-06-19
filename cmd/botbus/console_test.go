package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-proto/wire"
)

func updRoster(m rosterModel, msg tea.Msg) (rosterModel, bool) {
	return m.updateRoster(msg)
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
