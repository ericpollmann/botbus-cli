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

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
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

func ensureWorkspaceRoot(ctx context.Context, d hostagent.Deps, profilePath, wsName, user string) (agentstate.Agent, error) {
	root, ok, err := hostagent.GetByName(d.StatePath, wsName)
	if err != nil {
		return agentstate.Agent{}, err
	}
	if ok {
		// Reuse: re-register (no field changes) so a prior run that minted locally
		// but failed to reach the router self-heals (mirrors hostagent.EnsureRoot).
		root, err = hostagent.Update(ctx, d, wsName, hostagent.UpdateFields{})
		if err != nil {
			return agentstate.Agent{}, err
		}
	} else {
		root, err = hostagent.Create(ctx, d, hostagent.CreateOpts{Name: wsName}) // Parent="" => org-root
		if err != nil {
			return agentstate.Agent{}, err
		}
	}

	// Persist the org-root as the operator's profile root, preserving any existing
	// Framing (profile.Load returns a zero profile on first run).
	p, err := profile.Load(profilePath)
	if err != nil || p == nil {
		p = &profile.Profile{}
	}
	p.User = user
	p.Root = profile.Root{ID: root.ID, InboxChannel: root.InboxChannel, Key: root.Key}
	if err := profile.Save(profilePath, p); err != nil {
		return agentstate.Agent{}, err
	}
	if err := setActiveWorkspace(d.StatePath, root.ID); err != nil {
		return agentstate.Agent{}, err
	}
	return root, nil
}
