package main

// console.go — the roster screen sub-model for the hierarchical console: the
// operator's at-a-glance view of their agent tree (every agent, ● live / ○
// idle, name, focus) plus cursor navigation. The main TUI embeds this and owns
// mode switching + dip-in (Task 5); here we only do roster data + navigation +
// View rendering. No WebSocket, no mode switching.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
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

// scopeToWorkspace returns only the nodes in the subtree rooted at orgRootID
// (that node + every descendant by Parent chain). orgRootID=="" returns nodes
// unchanged (no active workspace → show everything, today's behavior). If
// orgRootID is set but not present in the roster (a stale or deregistered
// workspace), it also returns all nodes rather than stranding the operator with
// an empty console.
func scopeToWorkspace(nodes []wire.AgentNode, orgRootID string) []wire.AgentNode {
	if orgRootID == "" {
		return nodes
	}
	byID := make(map[string]wire.AgentNode, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if _, ok := byID[orgRootID]; !ok {
		return nodes // active workspace not in the roster → don't show an empty console
	}
	var out []wire.AgentNode
	for _, n := range nodes {
		// Walk the Parent chain up to len(nodes) hops (cycle cap) looking for the
		// org-root. The node itself counts (cur == orgRootID on the first check).
		cur := n.ID
		for hops := 0; hops <= len(nodes); hops++ {
			if cur == orgRootID {
				out = append(out, n)
				break
			}
			parent, ok := byID[cur]
			if !ok || parent.Parent == "" {
				break
			}
			cur = parent.Parent
		}
	}
	return out
}

// removeByID drops the node with id from the roster and clamps the cursor so it
// stays within bounds (a no-op if id isn't present).
func (m *rosterModel) removeByID(id string) {
	out := m.nodes[:0]
	for _, n := range m.nodes {
		if n.ID != id {
			out = append(out, n)
		}
	}
	m.nodes = out
	if m.cursor >= len(m.nodes) && m.cursor > 0 {
		m.cursor = len(m.nodes) - 1
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
	b.WriteString("\n" + hintStyle.Render("↑/↓ navigate · enter dip in · o onboard · d remove · esc quit"))
	return b.String()
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

	if !p.Configured() {
		// First run: hand off to the guided wizard (it sets up the profile +
		// workspace and ends in the live board). The operator re-runs `botbus`
		// afterward to open the console.
		runOnboard()
		return
	}
	rt := buildRuntime(p)

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

	// Scope the roster to the operator's active workspace (an org-root subtree).
	// A missing/unreadable state file or an empty active workspace leaves the
	// roster unchanged (show everything — today's behavior).
	if st, serr := agentstate.Load(agentstate.DefaultPath()); serr == nil {
		nodes = scopeToWorkspace(nodes, st.ActiveWorkspace)
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
	// When the TUI exits the signal/ctx cancel tears down runAll (→ context.Canceled,
	// the normal exit). Surface any OTHER error to stderr so a mid-session serve
	// failure isn't swallowed, leaving the TUI running with no MCP face.
	go func() {
		if err := runAll(ctx, rt, ln); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "botbus: runtime faces stopped:", err)
		}
	}()

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
	m.remove = func(node wire.AgentNode) (error, error) {
		return ops.Remove(context.Background(), node.ID)
	}
}
