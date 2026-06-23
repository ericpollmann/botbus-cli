package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFetchBoardDecodesColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/board" {
			t.Errorf("expected /board, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"in_progress":[{"task":"t1","title":"Build it","by":"eric"}],"blocked":[],"done":[{"task":"t0","title":"Done thing","by":"bot"}]}`))
	}))
	defer srv.Close()

	b, err := fetchBoard(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchBoard: %v", err)
	}
	if len(b.InProgress) != 1 || b.InProgress[0].Title != "Build it" || b.InProgress[0].By != "eric" {
		t.Fatalf("in_progress not decoded: %+v", b.InProgress)
	}
	if len(b.Done) != 1 || b.Done[0].Title != "Done thing" {
		t.Fatalf("done not decoded: %+v", b.Done)
	}
}

func TestFetchBoardErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fetchBoard(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestLiveBoardUpdateAppliesBoardAndErr(t *testing.T) {
	m := newLiveBoardModel(context.Background(), "https://x.botbus.ai/", "mythwork")

	// A board message populates the model and clears any prior error.
	got, _ := m.Update(boardMsg(boardView{InProgress: []boardCard{{Title: "Onboarding"}}}))
	m = got.(liveBoardModel)
	if !m.loaded || len(m.board.InProgress) != 1 {
		t.Fatalf("boardMsg should load the board, got loaded=%v board=%+v", m.loaded, m.board)
	}
	if v := m.View(); !strings.Contains(v, "Onboarding") {
		t.Fatalf("View should render the card title, got:\n%s", v)
	}

	// An error message sets err (and View shows a reconnect line, not a crash).
	got, _ = m.Update(boardErrMsg{err: context.DeadlineExceeded})
	m = got.(liveBoardModel)
	if m.err == nil {
		t.Fatal("boardErrMsg should set err")
	}
	if !strings.Contains(m.View(), "reconnect") {
		t.Fatalf("View should show a reconnect hint on error, got:\n%s", m.View())
	}
}

func TestLiveBoardTickReschedulesFetch(t *testing.T) {
	m := newLiveBoardModel(context.Background(), "https://x.botbus.ai/", "mythwork")
	_, cmd := m.Update(boardTickMsg{})
	if cmd == nil {
		t.Fatal("a tick should schedule the next fetch+tick (non-nil cmd)")
	}
}

func TestLiveBoardQuitKeys(t *testing.T) {
	m := newLiveBoardModel(context.Background(), "https://x.botbus.ai/", "mythwork")
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyRunes, Runes: []rune("q")},
	} {
		if _, cmd := m.Update(k); cmd == nil {
			t.Fatalf("key %v should return a quit command", k)
		}
	}
}
