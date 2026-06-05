package main

// history.go — client for the server's GET /history (and /audio/history)
// pagination endpoints. Used to seed the scrollback on connect and to page
// further back as the user scrolls up in the TUI (ui.go). The endpoint is
// stateless (any instance serves it from the shared buffer Redis), returning
// newest-first {"msgs":[{"id","d":base64}], "next":"<oldest-id|"">"}.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// histMsg is one buffered frame: its Redis stream ID and base64-encoded raw
// frame bytes (binary-safe, since the audio stream carries Opus).
type histMsg struct {
	ID string `json:"id"`
	D  string `json:"d"`
}

// histPage is one page of history. Next is the oldest ID in this page — pass
// it back as `before` to page further back; it is "" once the start of the
// buffer is reached.
type histPage struct {
	Msgs []histMsg `json:"msgs"`
	Next string    `json:"next"`
}

// historyTimeout bounds a single /history fetch so a slow buffer Redis can't
// hang the seed or a scroll-back request.
const historyTimeout = 5 * time.Second

// fetchHistory GETs <base><path> (path is "/history" or "/audio/history").
// base is the channel's HTTP origin (e.g. https://<id>.botbus.ai). before is
// the pagination cursor ("" for the newest page); n is the page size (the
// server clamps it). Returns the parsed page or an error.
func fetchHistory(ctx context.Context, base, path, before string, n int) (histPage, error) {
	u, err := url.Parse(base)
	if err != nil {
		return histPage{}, err
	}
	u.Path = path
	q := url.Values{}
	if before != "" {
		q.Set("before", before)
	}
	if n > 0 {
		q.Set("n", strconv.Itoa(n))
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return histPage{}, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return histPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return histPage{}, fmt.Errorf("history: %s", resp.Status)
	}
	var p histPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return histPage{}, err
	}
	return p, nil
}

// decodeHistFrame base64-decodes one entry's frame bytes, or nil on malformed
// input (the caller skips nils).
func decodeHistFrame(d string) []byte {
	b, err := base64.StdEncoding.DecodeString(d)
	if err != nil {
		return nil
	}
	return b
}

// histFramesChrono decodes a (newest-first) page into raw frames in
// chronological (oldest-first) order, skipping any that fail to decode.
func histFramesChrono(p histPage) [][]byte {
	out := make([][]byte, 0, len(p.Msgs))
	for i := len(p.Msgs) - 1; i >= 0; i-- {
		if f := decodeHistFrame(p.Msgs[i].D); f != nil {
			out = append(out, f)
		}
	}
	return out
}
