package main

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-proto/wire"
)

// makeRemoveRecorder returns a remove closure that records the node it was
// called with and returns canned (routerErr, err) values.
func makeRemoveRecorder(routerErr, err error) (func(wire.AgentNode) (error, error), *wire.AgentNode) {
	var called wire.AgentNode
	fn := func(n wire.AgentNode) (error, error) {
		called = n
		return routerErr, err
	}
	return fn, &called
}

// keyRune builds a tea.KeyMsg for a single rune.
func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// threeNodes returns a roster with three nodes for use in remove tests.
func threeNodes() []wire.AgentNode {
	return []wire.AgentNode{
		{ID: "id-root", Name: "root", InboxChannel: "i-root"},
		{ID: "id-c1", Name: "myth-compiler", Parent: "id-root", InboxChannel: "i-c1"},
		{ID: "id-c2", Name: "myth-cli", Parent: "id-root", InboxChannel: "i-c2"},
	}
}

// TestRemoveDKeyBeginsConfirm: pressing `d` on a selected node sets
// confirmingDelete=true and captures the target node.
func TestRemoveDKeyBeginsConfirm(t *testing.T) {
	m := newConsoleModel(threeNodes())
	m.remove = func(wire.AgentNode) (error, error) { return nil, nil }

	// Navigate to the second node.
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = updConsole(m, keyRune('d'))

	if !m.confirmingDelete {
		t.Fatal("d should set confirmingDelete=true")
	}
	if m.deleteTarget.ID != "id-c1" {
		t.Fatalf("deleteTarget should be the selected node (id-c1), got %q", m.deleteTarget.ID)
	}
}

// TestRemoveDThenYRemovesNode: `d` then `y` calls remove, drops the node,
// and clears confirmingDelete.
func TestRemoveDThenYRemovesNode(t *testing.T) {
	removeFn, called := makeRemoveRecorder(nil, nil)
	m := newConsoleModel(threeNodes())
	m.remove = removeFn

	// Navigate to second node and press d.
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = updConsole(m, keyRune('d'))
	if !m.confirmingDelete {
		t.Fatal("expected confirmingDelete after d")
	}

	// Press y to confirm.
	m, _ = updConsole(m, keyRune('y'))

	if m.confirmingDelete {
		t.Fatal("confirmingDelete should be false after y")
	}
	// remove should have been called with the target.
	if called.ID != "id-c1" {
		t.Fatalf("remove called with wrong node: got %q, want id-c1", called.ID)
	}
	// Node must be gone from the roster.
	for _, n := range m.roster.nodes {
		if n.ID == "id-c1" {
			t.Fatal("id-c1 should have been removed from roster.nodes")
		}
	}
	// onboardMsg should say "removed".
	if m.onboardMsg == "" {
		t.Fatal("onboardMsg should contain a 'removed' confirmation")
	}
}

// TestRemoveDThenNAborts: `d` then `n` cancels — node stays, remove not called.
func TestRemoveDThenNAborts(t *testing.T) {
	called := false
	m := newConsoleModel(threeNodes())
	m.remove = func(wire.AgentNode) (error, error) { called = true; return nil, nil }

	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = updConsole(m, keyRune('d'))
	m, _ = updConsole(m, keyRune('n'))

	if m.confirmingDelete {
		t.Fatal("confirmingDelete should be false after n")
	}
	if called {
		t.Fatal("remove must not be called after n")
	}
	// Node must still be present.
	found := false
	for _, n := range m.roster.nodes {
		if n.ID == "id-c1" {
			found = true
		}
	}
	if !found {
		t.Fatal("id-c1 should still be in the roster after n")
	}
}

// TestRemoveErrorKeepsNode: when remove returns a non-nil err, the node stays
// and onboardMsg contains the error text.
func TestRemoveErrorKeepsNode(t *testing.T) {
	boom := errors.New("not local")
	m := newConsoleModel(threeNodes())
	m.remove = func(wire.AgentNode) (error, error) { return nil, boom }

	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = updConsole(m, keyRune('d'))
	m, _ = updConsole(m, keyRune('y'))

	if m.confirmingDelete {
		t.Fatal("confirmingDelete should be false after y even on error")
	}
	// Node must still be present.
	found := false
	for _, n := range m.roster.nodes {
		if n.ID == "id-c1" {
			found = true
		}
	}
	if !found {
		t.Fatal("id-c1 should remain in roster when remove errors")
	}
	if m.onboardMsg == "" {
		t.Fatal("onboardMsg should contain the error text")
	}
	// Message should mention the error.
	if !contains(m.onboardMsg, "not local") {
		t.Fatalf("onboardMsg should contain error text 'not local', got %q", m.onboardMsg)
	}
}

// contains is a simple substring helper for test assertions.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// TestRemoveNilRemoveIsNoop: `d` is a no-op when m.remove==nil (direct-chat path).
func TestRemoveNilRemoveIsNoop(t *testing.T) {
	m := newConsoleModel(threeNodes())
	// m.remove stays nil — simulates direct-chat path.

	m, _ = updConsole(m, keyRune('d'))

	if m.confirmingDelete {
		t.Fatal("d should be a no-op when m.remove is nil — confirmingDelete must stay false")
	}
}

// TestRemoveByID unit-tests rosterModel.removeByID directly.
func TestRemoveByID(t *testing.T) {
	nodes := []wire.AgentNode{
		{ID: "a", Name: "alpha"},
		{ID: "b", Name: "beta"},
		{ID: "c", Name: "gamma"},
	}
	r := newRosterModel(nodes)
	r.cursor = 2 // point at gamma

	r.removeByID("b")

	if len(r.nodes) != 2 {
		t.Fatalf("expected 2 nodes after removeByID, got %d", len(r.nodes))
	}
	for _, n := range r.nodes {
		if n.ID == "b" {
			t.Fatal("node b should be gone")
		}
	}
	// cursor was 2, now only 2 nodes remain (0,1) → must clamp to 1.
	if r.cursor != 1 {
		t.Fatalf("cursor should clamp to 1 after removal, got %d", r.cursor)
	}
}

// TestRemoveByIDMissingIsNoop: removeByID with an unknown id does nothing.
func TestRemoveByIDMissingIsNoop(t *testing.T) {
	r := newRosterModel([]wire.AgentNode{{ID: "x", Name: "x"}})
	r.cursor = 0
	r.removeByID("nonexistent")
	if len(r.nodes) != 1 {
		t.Fatalf("removeByID with unknown id should be a no-op, got %d nodes", len(r.nodes))
	}
	if r.cursor != 0 {
		t.Fatalf("cursor should stay 0, got %d", r.cursor)
	}
}

// TestRemoveRouterErrSurfacesInMsg: when remove returns a routerErr but no err,
// the node IS removed and the msg mentions the router error.
func TestRemoveRouterErrSurfacesInMsg(t *testing.T) {
	routerBoom := errors.New("router timeout")
	m := newConsoleModel(threeNodes())
	m.remove = func(wire.AgentNode) (error, error) { return routerBoom, nil }

	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = updConsole(m, keyRune('d'))
	m, _ = updConsole(m, keyRune('y'))

	// Node should be GONE (local remove succeeded).
	for _, n := range m.roster.nodes {
		if n.ID == "id-c1" {
			t.Fatal("node should be removed even when only routerErr is set")
		}
	}
	// Message should mention the router error.
	if !contains(m.onboardMsg, "router") {
		t.Fatalf("onboardMsg should mention router error, got %q", m.onboardMsg)
	}
}
