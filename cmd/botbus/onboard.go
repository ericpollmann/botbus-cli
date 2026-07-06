package main

// onboard.go — the self-documenting onboarding wizard: name a workspace, connect
// this session, set a directive, invite teammates, add an agent, then watch the
// live board. Steps 1-5 are imperative prompts (this file); step 6 hands off to
// liveBoardModel (board_live.go). Logic reuses hostagent/workspace/onboardChildOps.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
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
		// but failed to reach the router self-heals (Register is idempotent).
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
	if err != nil {
		return agentstate.Agent{}, err
	}
	if p == nil {
		p = &profile.Profile{}
	}

	// Mint (once) the workspace source channel and bind it to the root so the
	// router routes messages arriving on it only within this workspace. Reuse the
	// existing source when re-onboarding the SAME root, so a re-run doesn't mint a
	// fresh channel; AddSource is idempotent for the same root+channel regardless.
	// Bind BEFORE persisting so a failed bind never leaves a persisted-but-unbound
	// source.
	source := p.Root.Source
	if p.Root.ID != root.ID || source == "" {
		source, err = d.Hub.MintChannel(ctx)
		if err != nil {
			return agentstate.Agent{}, err
		}
	}
	if err := d.Control.AddSource(ctx, root.ID, root.Key, source); err != nil {
		return agentstate.Agent{}, err
	}

	p.User = user
	p.Root = profile.Root{ID: root.ID, InboxChannel: root.InboxChannel, Key: root.Key, Source: source}
	if err := profile.Save(profilePath, p); err != nil {
		return agentstate.Agent{}, err
	}
	if err := setActiveWorkspace(d.StatePath, root.ID); err != nil {
		return agentstate.Agent{}, err
	}
	// Point the daemon's outbound channel at the workspace source so every agent's
	// `send` publishes there (the router anchors that channel to this workspace).
	if err := setOutboundChannel(d.StatePath, source); err != nil {
		return agentstate.Agent{}, err
	}
	return root, nil
}

// ask prints prompt to out and returns the next trimmed input line.
func ask(r *bufio.Reader, out io.Writer, prompt string) string {
	fmt.Fprint(out, prompt)
	line, _ := readLine(r) // readLine is defined in console.go; EOF yields ""
	return strings.TrimSpace(line)
}

// rootConnect builds local-MCP connect instructions for the operator's root,
// mirroring the daemon's CreateChild shape (http://<addr>/a/<key>).
func rootConnect(addr string, root agentstate.Agent) daemon.ConnectInstructions {
	endpoint := fmt.Sprintf("http://%s/a/%s", addr, root.Key)
	return daemon.ConnectInstructions{
		MCPCommand:  fmt.Sprintf("claude mcp add --transport http %s %s", root.Name, endpoint),
		MCPEndpoint: endpoint,
		ChannelURL:  fmt.Sprintf("https://%s.%s/", root.InboxChannel, domain),
	}
}

// onboardSteps runs the imperative guided steps 1-5 and returns the workspace
// root channel URL the caller watches in the live board (step 6). rebuild
// produces the runtime Ops bound to the freshly-saved profile so step 5's
// CreateChild parents the new agent under the workspace root.
func onboardSteps(in io.Reader, out io.Writer, d hostagent.Deps, profilePath string, rebuild func(*profile.Profile) daemon.Ops) (string, error) {
	r := bufio.NewReader(in)
	ctx := context.Background()

	fmt.Fprint(out, "botbus onboarding — let's set up your workspace.\n\n")

	user := ask(r, out, "Your name: ")
	if user == "" {
		return "", fmt.Errorf("name is required")
	}
	wsName := ask(r, out, "Workspace name: ")
	if wsName == "" {
		return "", fmt.Errorf("workspace name is required")
	}

	// Step 1: create (or reuse) the workspace org-root = the operator's root.
	root, err := ensureWorkspaceRoot(ctx, d, profilePath, wsName, user)
	if err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	channelURL := fmt.Sprintf("https://%s.%s/", root.InboxChannel, domain)
	fmt.Fprintf(out, "\n✓ workspace %q is live: %s\n", wsName, channelURL)

	// Rebuild Ops against the saved profile so CreateChild sees the new root.
	p, _ := profile.Load(profilePath)
	ops := rebuild(p)

	// Step 2: connect this session (local-MCP paste prompt + terminal fallback).
	inst := rootConnect(ops.Addr(), root)
	fmt.Fprintln(out, "\n── Connect THIS coding-agent session ──")
	fmt.Fprintln(out, localPastePrompt(wsName, "workspace owner", inst))
	fmt.Fprintf(out, "\n(terminal fallback: %s)\n", inst.MCPCommand)

	// Step 3: workspace directive (optional).
	directive := ask(r, out, "\nWorkspace directive (optional, Enter to skip): ")
	if directive != "" {
		if _, uerr := hostagent.Update(ctx, d, wsName, hostagent.UpdateFields{Focus: &directive}); uerr != nil {
			fmt.Fprintln(out, "  (couldn't set directive:", uerr, ")")
		} else {
			if p2, lerr := profile.Load(profilePath); lerr == nil && p2 != nil {
				// Intentionally mirrored: Focus drives the root-agent briefing/roster;
				// Framing is injected into child-agent welcome messages. Both must stay in sync.
				p2.Framing = directive
				_ = profile.Save(profilePath, p2)
			}
			fmt.Fprintln(out, "✓ directive set")
		}
	}

	// Step 4: invite teammates (loop until a blank name).
	for {
		teammate := ask(r, out, "\nInvite a teammate (name, Enter to finish): ")
		if teammate == "" {
			break
		}
		joinURL, ierr := workspaceInvite(ctx, d, teammate, wsName)
		if ierr != nil {
			fmt.Fprintln(out, "  (invite failed:", ierr, ")")
			continue
		}
		fmt.Fprintln(out, invitePastePrompt(teammate, joinURL))
	}

	// Step 5: add a standing agent (optional).
	agentName := ask(r, out, "\nAdd a standing agent/role (name, Enter to skip): ")
	if agentName != "" {
		focus := ask(r, out, "  Its focus: ")
		childInst, aerr := onboardChildOps(ctx, ops, agentName, focus)
		if aerr != nil {
			fmt.Fprintln(out, "  (couldn't create agent:", aerr, ")")
		} else {
			fmt.Fprintf(out, "\nConnect a new coding-agent session as %s:\n%s\n", agentName, localPastePrompt(agentName, focus, childInst))
		}
	}

	return channelURL, nil
}

// runOnboard is the no-args wizard entrypoint: guided setup (steps 1-5) then the
// live board (step 6). Used for bare-botbus first-run and `botbus onboard`.
func runOnboard() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	profilePath := profile.DefaultPath()
	deps := realDeps()
	rebuild := func(p *profile.Profile) daemon.Ops { return buildRuntime(p) }

	channelURL, err := onboardSteps(os.Stdin, os.Stdout, deps, profilePath, rebuild)
	if err != nil {
		fmt.Fprintln(os.Stderr, "onboard:", err)
		os.Exit(1)
	}

	// Seed one card so the board isn't empty on first paint (best-effort, time-boxed
	// so a slow/hung hub can't stall the hand-off into the live board).
	if p, lerr := profile.Load(profilePath); lerr == nil && p != nil {
		by := p.User
		if by == "" {
			by = "you"
		}
		sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
		if serr := seedSampleTask(sctx, channelURL, by); serr != nil {
			fmt.Fprintln(os.Stderr, "(seed skipped:", serr, ")")
		}
		scancel()
	}

	fmt.Fprintln(os.Stdout, "\nOpening your live board — watch tasks appear. (q to quit)")
	m := newLiveBoardModel(ctx, channelURL, "your workspace")
	if _, rerr := tea.NewProgram(m, tea.WithAltScreen()).Run(); rerr != nil {
		fmt.Fprintln(os.Stderr, rerr)
	}
	fmt.Fprintln(os.Stdout, "Done. Run `botbus` anytime to open your console (it serves the local MCP your agents use).")
}
