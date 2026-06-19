package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// newScrollTestModel builds a model with a fixed size and the welcome popup
// dismissed, so key handling reaches the scroll/chat logic. XDG is pointed at
// a temp dir so isWelcomed() doesn't touch the real config.
func newScrollTestModel(t *testing.T) model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := newModel("ch.botbus.ai", "https://ch.botbus.ai", "me", false,
		make(chan []byte), make(chan connState), make(chan []byte, 1), nil)
	m.welcome.visible = false
	m.w, m.h = 80, 24
	return m
}

func upd(m model, msg tea.Msg) (model, tea.Cmd) {
	nm, cmd := m.Update(msg)
	return nm.(model), cmd
}

func TestSeedMsgAppendsAndSetsCursor(t *testing.T) {
	m := newScrollTestModel(t)
	m, _ = upd(m, seedMsg{frames: [][]byte{[]byte("a: 1"), []byte("b: 2"), []byte("c: 3")}, next: "5-0"})
	if len(m.msgs) != 3 {
		t.Fatalf("want 3 seeded msgs, got %d", len(m.msgs))
	}
	if m.oldestID != "5-0" {
		t.Errorf("oldestID = %q, want 5-0", m.oldestID)
	}
	if m.noMoreHist {
		t.Error("noMoreHist should be false when a cursor was returned")
	}
	// Oldest-first order preserved (a, then b, then c).
	if !strings.Contains(m.msgs[0], "1") || !strings.Contains(m.msgs[2], "3") {
		t.Errorf("seed order wrong: %v", m.msgs)
	}
}

func TestSeedMsgNoCursorMeansBufferStart(t *testing.T) {
	m := newScrollTestModel(t)
	m, _ = upd(m, seedMsg{frames: [][]byte{[]byte("a: 1")}, next: ""})
	if m.oldestID != "" || !m.noMoreHist {
		t.Errorf("seed with empty next → oldestID=%q noMoreHist=%v, want \"\"/true", m.oldestID, m.noMoreHist)
	}
}

func TestOlderMsgPrependsAndAnchors(t *testing.T) {
	m := newScrollTestModel(t)
	m.msgs = []string{"x", "y"}
	m.scrollOff = 4
	m.oldestID = "10-0"
	m, _ = upd(m, olderMsg{frames: [][]byte{[]byte("a: 1"), []byte("b: 2")}, next: "3-0"})
	if len(m.msgs) != 4 {
		t.Fatalf("want 4 msgs after prepend, got %d", len(m.msgs))
	}
	// Older frames go ABOVE the existing ones, oldest-first.
	if !strings.Contains(m.msgs[0], "1") || m.msgs[2] != "x" {
		t.Errorf("prepend order wrong: %v", m.msgs)
	}
	if m.oldestID != "3-0" {
		t.Errorf("oldestID = %q, want 3-0", m.oldestID)
	}
	// Two one-row messages added → offset grows by 2 to stay anchored.
	if m.scrollOff != 6 {
		t.Errorf("scrollOff = %d, want 6 (4 + 2 added rows)", m.scrollOff)
	}
}

func TestOlderMsgEmptyStopsPaging(t *testing.T) {
	m := newScrollTestModel(t)
	m.histLoading = true
	m, _ = upd(m, olderMsg{frames: nil, next: ""})
	if m.histLoading || !m.noMoreHist {
		t.Errorf("empty older page → histLoading=%v noMoreHist=%v, want false/true", m.histLoading, m.noMoreHist)
	}
}

func TestOlderMsgFailedReenablesFetch(t *testing.T) {
	m := newScrollTestModel(t)
	m.histLoading = true
	m, _ = upd(m, olderMsg{failed: true})
	if m.histLoading {
		t.Error("failed older fetch should clear histLoading so a later scroll retries")
	}
	if m.noMoreHist {
		t.Error("a failed fetch must not mark the buffer exhausted")
	}
}

func TestIncomingAnchorsWhenScrolledUp(t *testing.T) {
	m := newScrollTestModel(t)
	m.scrollOff = 3
	m, _ = upd(m, incoming{data: []byte("z: hello")})
	if m.scrollOff != 4 {
		t.Errorf("scrollOff = %d, want 4 (anchored: +1 row for the new line)", m.scrollOff)
	}
}

func TestIncomingAtBottomStaysAtBottom(t *testing.T) {
	m := newScrollTestModel(t)
	m.scrollOff = 0
	m, _ = upd(m, incoming{data: []byte("z: hi")})
	if m.scrollOff != 0 {
		t.Errorf("at bottom a new line must not scroll us up; scrollOff = %d", m.scrollOff)
	}
}

func TestOwnMessageJumpsToBottom(t *testing.T) {
	m := newScrollTestModel(t)
	m.input.SetValue("hello there")
	m.scrollOff = 5
	m, _ = upd(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.scrollOff != 0 {
		t.Errorf("sending should snap to bottom; scrollOff = %d", m.scrollOff)
	}
	if len(m.msgs) != 1 {
		t.Errorf("own line should be appended locally; got %d msgs", len(m.msgs))
	}
}

func TestPgUpScrollsAndEndReturns(t *testing.T) {
	m := newScrollTestModel(t)
	for i := 0; i < 40; i++ {
		m.msgs = append(m.msgs, "x")
	}
	m, _ = upd(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.scrollOff <= 0 {
		t.Errorf("PgUp should scroll up; scrollOff = %d", m.scrollOff)
	}
	m, _ = upd(m, tea.KeyMsg{Type: tea.KeyEnd})
	if m.scrollOff != 0 {
		t.Errorf("End should return to the bottom; scrollOff = %d", m.scrollOff)
	}
}

func TestPgUpAtTopTriggersLoadOlder(t *testing.T) {
	m := newScrollTestModel(t)
	for i := 0; i < 40; i++ {
		m.msgs = append(m.msgs, "x")
	}
	m.scrollOff = m.maxScroll() // already at the top

	// No cursor → no fetch.
	m.oldestID = ""
	m2, cmd := upd(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if cmd != nil || m2.histLoading {
		t.Errorf("no cursor → must not fetch; cmd=%v loading=%v", cmd != nil, m2.histLoading)
	}

	// With a cursor → fetch fires.
	m.oldestID = "9-0"
	m3, cmd := upd(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if cmd == nil || !m3.histLoading {
		t.Errorf("at top with cursor → should fetch older; cmd=%v loading=%v", cmd != nil, m3.histLoading)
	}
}

func TestMaybeLoadOlderGatedWhileLoading(t *testing.T) {
	m := newScrollTestModel(t)
	for i := 0; i < 40; i++ {
		m.msgs = append(m.msgs, "x")
	}
	m.scrollOff = m.maxScroll()
	m.oldestID = "9-0"
	m.histLoading = true // already fetching
	_, cmd := upd(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if cmd != nil {
		t.Error("must not start a second fetch while one is in flight")
	}
}
