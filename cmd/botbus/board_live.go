package main

// board_live.go — the live task board the onboarding wizard ends in. fetchBoard
// GETs a channel's /board JSON (the hub aggregates the whole subtree, so the
// workspace org-root's board is the whole-workspace view); liveBoardModel (Task 3)
// polls it on a tick and bubbletea redraws.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// boardCard mirrors the fields of the hub's /board card the live view renders.
type boardCard struct {
	Task  string `json:"task"`
	Title string `json:"title"`
	Note  string `json:"note"`
	By    string `json:"by"`
}

// boardView mirrors the hub's /board JSON: three status columns of cards.
type boardView struct {
	InProgress []boardCard `json:"in_progress"`
	Blocked    []boardCard `json:"blocked"`
	Done       []boardCard `json:"done"`
}

// fetchBoard GETs <channelURL>/board and decodes the JSON board. The CLI
// User-Agent ensures the hub returns JSON (browsers get HTML).
func fetchBoard(ctx context.Context, channelURL string) (boardView, error) {
	u := strings.TrimRight(channelURL, "/") + "/board"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return boardView{}, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return boardView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return boardView{}, fmt.Errorf("board: HTTP %d", resp.StatusCode)
	}
	var b boardView
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return boardView{}, err
	}
	return b, nil
}

// boardPollInterval is how often the live board refetches /board. Human cadence;
// a WS-triggered refetch is a later upgrade.
const boardPollInterval = 2 * time.Second

type boardMsg boardView
type boardErrMsg struct{ err error }
type boardTickMsg struct{}

// liveBoardModel polls a channel's /board and redraws on each result. ctx scopes
// the HTTP fetches so program exit cancels in-flight requests.
type liveBoardModel struct {
	ctx    context.Context
	url    string
	title  string
	board  boardView
	loaded bool
	err    error
}

func newLiveBoardModel(ctx context.Context, channelURL, title string) liveBoardModel {
	return liveBoardModel{ctx: ctx, url: channelURL, title: title}
}

func (m liveBoardModel) Init() tea.Cmd { return tea.Batch(m.fetchCmd(), m.tickCmd()) }

func (m liveBoardModel) fetchCmd() tea.Cmd {
	return func() tea.Msg {
		b, err := fetchBoard(m.ctx, m.url)
		if err != nil {
			return boardErrMsg{err: err}
		}
		return boardMsg(b)
	}
}

func (m liveBoardModel) tickCmd() tea.Cmd {
	return tea.Tick(boardPollInterval, func(time.Time) tea.Msg { return boardTickMsg{} })
}

func (m liveBoardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case boardTickMsg:
		return m, tea.Batch(m.fetchCmd(), m.tickCmd())
	case boardMsg:
		m.board = boardView(t)
		m.loaded = true
		m.err = nil
		return m, nil
	case boardErrMsg:
		m.err = t.err
		return m, nil
	case tea.KeyMsg:
		switch t.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m liveBoardModel) View() string {
	var b strings.Builder
	b.WriteString(barStyle.Render("BOTBUS · "+m.title+" · live board") + "\n\n")
	cols := []struct {
		name  string
		cards []boardCard
	}{
		{"In progress", m.board.InProgress},
		{"Blocked", m.board.Blocked},
		{"Done", m.board.Done},
	}
	for _, c := range cols {
		b.WriteString(fmt.Sprintf("%s (%d)\n", c.name, len(c.cards)))
		for _, card := range c.cards {
			title := card.Title
			if title == "" {
				title = card.Task
			}
			line := "  • " + title
			if card.By != "" {
				line += hintStyle.Render("  — " + card.By)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	if m.err != nil {
		b.WriteString(hintStyle.Render("reconnecting…") + "\n")
	} else if !m.loaded {
		b.WriteString(hintStyle.Render("loading…") + "\n")
	}
	b.WriteString(hintStyle.Render("auto-refreshes every 2s · q quit"))
	return b.String()
}
