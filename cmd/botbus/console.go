package main

// console.go — the roster screen sub-model for the hierarchical console: the
// operator's at-a-glance view of their agent tree (every agent, ● live / ○
// idle, name, focus) plus cursor navigation. The main TUI embeds this and owns
// mode switching + dip-in (Task 5); here we only do roster data + navigation +
// View rendering. No WebSocket, no mode switching.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/wire"
)

type viewMode int

const (
	modeRoster viewMode = iota
	modeChat
)

// rosterModel is the tree/roster screen: the agent list + a cursor.
type rosterModel struct {
	nodes  []wire.AgentNode
	cursor int
}

func newRosterModel(nodes []wire.AgentNode) rosterModel {
	return rosterModel{nodes: nodes}
}

// newConsoleModel builds a console rooted in roster mode. It initializes the
// same maps/fields newModel does that the chat path relies on (the seenColors
// map and the input textarea), so dipping into chat mode doesn't hit a nil map
// or a zero-value textarea. The real WS channels + name are bound on dip-in
// (Task 6); here startChat/stopChat stay nil unless injected by a test.
func newConsoleModel(nodes []wire.AgentNode) model {
	return model{
		mode:       modeRoster,
		roster:     newRosterModel(nodes),
		state:      stConnecting,
		input:      newChatInput(""),
		seenColors: map[int]time.Time{},
	}
}

func (m rosterModel) selected() wire.AgentNode {
	if len(m.nodes) == 0 {
		return wire.AgentNode{}
	}
	return m.nodes[m.cursor]
}

// updateRoster handles navigation keys; returns the model and whether the user
// chose to dip into the selected node (enter).
func (m rosterModel) updateRoster(msg tea.Msg) (rosterModel, bool) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.nodes)-1 {
				m.cursor++
			}
		case "enter":
			return m, true
		}
	}
	return m, false
}

// updateOnboard drives the inline name → focus onboarding prompt. It reuses the
// chat textarea as a single-line capture: enter advances the step (name → focus
// → mint), esc aborts back to the plain roster. On the focus step's enter it
// calls the injected onboard action and shows the connect URL (or error).
func (m model) updateOnboard(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		// Non-key messages (window size, ticks) fall through to the input update
		// so the textarea stays sized correctly.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	switch k.String() {
	case "ctrl+c":
		// ctrl+c is the universal quit, even mid-onboard.
		return m, tea.Quit
	case "esc":
		// esc only aborts the onboarding prompt, returning to the plain roster.
		m.onboardState = onboardOff
		m.onboardName = ""
		m.input.Reset()
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		switch m.onboardState {
		case onboardAskName:
			if val == "" {
				m.onboardMsg = "name is required"
				return m, nil
			}
			m.onboardName = val
			m.onboardState = onboardAskFocus
			return m, nil
		case onboardAskFocus:
			url, err := m.onboard(m.onboardName, val)
			m.onboardState = onboardOff
			if err != nil {
				m.onboardMsg = "onboard failed: " + err.Error()
				m.onboardName = ""
				return m, nil
			}
			m.onboardMsg = "tell your agent to connect to " + url
			m.onboardName = ""
			return m, nil
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m rosterModel) View() string {
	var b strings.Builder
	b.WriteString(barStyle.Render("BOTBUS · your agents") + "\n\n")
	for i, n := range m.nodes {
		marker := "○"
		if n.Live {
			marker = "●"
		}
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		indent := ""
		if n.Parent != "" {
			indent = "  "
		}
		line := fmt.Sprintf("%s%s%s %s", cursor, indent, marker, n.Name)
		if n.Focus != "" {
			line += hintStyle.Render("  — " + n.Focus)
		}
		b.WriteString(paletteStyle(nameColor(n.Name)).Render(line) + "\n")
	}
	b.WriteString("\n" + hintStyle.Render("↑/↓ navigate · enter dip in · o onboard · esc quit"))
	return b.String()
}

// firstRunOps is the one-time operator setup: prompt for the operator's name
// and the standing framing (read from in, prompts written to out), mint the
// root agent via ops.EnsureRoot, persist the profile, and return it. Factored
// pure (io.Reader/io.Writer + injected daemon.Ops + an explicit profile path)
// so it's unit-testable.
func firstRunOps(in io.Reader, out io.Writer, ops daemon.Ops, profilePath string) (*profile.Profile, error) {
	r := bufio.NewReader(in)
	fmt.Fprintln(out, "Welcome to the botbus console — one-time setup.")
	fmt.Fprint(out, "Your name: ")
	user, err := readLine(r)
	if err != nil {
		return nil, fmt.Errorf("read name: %w", err)
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, fmt.Errorf("name is required")
	}
	fmt.Fprint(out, "Standing framing (how you work, injected into every agent's welcome): ")
	framing, err := readLine(r)
	if err != nil {
		return nil, fmt.Errorf("read framing: %w", err)
	}
	framing = strings.TrimSpace(framing)

	// EnsureRoot (not CreateRoot) so a first-run that minted a local root but
	// then failed to Register (flaky router) is idempotent: the next run reuses
	// the existing local root and re-registers it instead of dying with "already
	// exists".
	root, err := ops.EnsureRoot(context.Background())
	if err != nil {
		return nil, fmt.Errorf("create root agent: %w", err)
	}

	p := &profile.Profile{
		User:    user,
		Framing: framing,
		Root: profile.Root{
			ID:           root.ID,
			InboxChannel: root.InboxChannel,
			Key:          root.Key,
		},
	}
	if err := profile.Save(profilePath, p); err != nil {
		return nil, fmt.Errorf("save profile: %w", err)
	}
	fmt.Fprintf(out, "Root channel ready: https://%s.%s\n", root.InboxChannel, domain)
	return p, nil
}

// readLine reads a single line (without the trailing newline) from r. io.EOF
// with content already read is treated as a complete final line.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// onboardChildOps creates a child via the shared Ops core and returns the
// operator-facing connect instruction (MCP-first, channel URL fallback).
func onboardChildOps(ctx context.Context, ops daemon.Ops, name, focus string) (string, error) {
	_, inst, err := ops.CreateChild(ctx, name, focus)
	if err != nil {
		return "", err
	}
	return inst.MCPCommand + "\n(or raw: " + inst.ChannelURL + ")", nil
}

// runConsole is the no-args entrypoint: load (or first-run create) the operator
// profile, fetch the agent roster from the router, and launch the hierarchical
// console TUI with real dip-in WS wiring.
func runConsole() {
	// Root every console context in a signal-canceled context (mirrors main()'s
	// direct-chat path) so SIGINT/SIGTERM cancels the live dip's WebSocket
	// cleanly on exit rather than leaving the socket goroutine running.
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	profilePath := profile.DefaultPath()
	p, err := profile.Load(profilePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "profile:", err)
		os.Exit(1)
	}

	// Build the one runtime (Ops + faces share it). buildRuntime is defined in
	// cmd/botbus/runtime.go (Task 9 adds the single-runtime guard).
	rt := buildRuntime(p)
	if !p.Configured() {
		p, err = firstRunOps(os.Stdin, os.Stdout, rt, profilePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "setup:", err)
			os.Exit(1)
		}
		rt = buildRuntime(p) // rebuild with the now-populated profile
	}

	nodes, err := rt.Roster(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "roster unavailable (is the router deployed?):", err)
		// Fall back to a single-node roster (just the root) so the console still
		// opens and the operator can at least dip into / onboard from the root.
		nodes = []wire.AgentNode{{
			ID:           p.Root.ID,
			Name:         "root",
			InboxChannel: p.Root.InboxChannel,
		}}
	}

	// Bind the MCP port before launching the TUI — a conflict fails fast with a
	// clean message rather than mid-UI, and holds the port as the single-runtime
	// mutex for the entire session.
	ln, err := ensureSingleRuntime(rt.Addr())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Start inbox loops + MCP mux in the background; TUI runs in the foreground.
	// When the TUI exits the signal/ctx cancel tears down runAll.
	go func() { _ = runAll(ctx, rt, ln) }()

	m := newConsoleModel(nodes)
	wireConsoleChat(ctx, &m, rt) // onboard closure now calls onboardChildOps(ctx, rt, ...)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// wireConsoleChat installs the real dip-in WS lifecycle hooks on the console
// model. startChat dials a live text WebSocket for the selected agent's inbox
// channel and returns the fresh transport channels for Update to bind; stopChat
// cancels that WS. The cancel is shared between the two closures via a captured
// variable so stopChat can tear down whatever startChat last opened.
func wireConsoleChat(parent context.Context, m *model, ops daemon.Ops) {
	var cancel context.CancelFunc
	name := resolveName()
	m.startChat = func(channel string) chatSession {
		// Tear down any prior dip before opening a new one (defensive — Update
		// always stops before re-dipping, but a stale goroutine here would leak).
		if cancel != nil {
			cancel()
		}
		// The inbox is a bare channel id → https://<id>.botbus.ai/.
		httpURL, rerr := resolveURL(channel)
		if rerr != nil {
			return chatSession{}
		}
		// Child of the signal-canceled parent so program exit (SIGINT/SIGTERM)
		// cancels the live dip's socket cleanly.
		ctx, c := context.WithCancel(parent)
		cancel = c
		recv := make(chan []byte, 64)
		send := make(chan []byte, 16)
		states := make(chan connState, 4)
		seedCh := make(chan seedMsg, 1)
		histBase := strings.TrimRight(httpURL, "/")
		textURL, _ := channelStreamURLs(httpURL)
		go runWSText(ctx, textURL, histBase, recv, send, states, seedCh)
		return chatSession{
			recv: recv, states: states, send: send, seed: seedCh,
			name: name, host: hostFromURL(httpURL), histBase: histBase,
		}
	}
	m.stopChat = func() {
		if cancel != nil {
			cancel()
			cancel = nil
		}
	}
	m.onboard = func(name, focus string) (string, error) {
		return onboardChildOps(context.Background(), ops, name, focus)
	}
}
