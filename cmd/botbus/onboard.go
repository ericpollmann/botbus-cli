package main

// onboard.go — the self-documenting onboarding wizard: name a workspace, connect
// this session, set a directive, invite teammates, add an agent, then watch the
// live board. Steps 1-5 are imperative prompts (this file); step 6 hands off to
// liveBoardModel (board_live.go). Logic reuses hostagent/workspace/onboardChildOps.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// seedSampleTask posts one task.started event frame to channelURL so the live
// board shows a card immediately. Best-effort: the caller logs any error and
// continues — a failed seed must not abort onboarding.
func seedSampleTask(ctx context.Context, channelURL, byName string) error {
	ev := struct {
		V     int    `json:"v"`
		Type  string `json:"type"`
		Task  string `json:"task"`
		Title string `json:"title"`
		By    string `json:"by"`
	}{1, "task.started", "onboarding", "Onboarding complete — you're live", byName}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	frame := byName + ": " + string(body)
	u := strings.TrimRight(channelURL, "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(frame))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("seed: HTTP %d", resp.StatusCode)
	}
	return nil
}
