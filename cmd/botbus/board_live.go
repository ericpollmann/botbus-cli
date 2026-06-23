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
