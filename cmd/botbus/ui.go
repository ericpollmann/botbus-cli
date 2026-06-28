package main

// ui.go — bubbletea model + Init/Update/View for the interactive chat TUI.
// Pure rendering helpers (palette, nameColor, parseMsg, renderSlash,
// visualRows) live in protocol.go.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-proto/wire"
)

const (
	dotTTL       = 5 * time.Minute
	spinSpeed    = 150 * time.Millisecond
	quitHint     = "Esc quit · PgUp history · /me · /dm · /compact"
	maxInputRows = 8
)

var spinFrames = []string{"◴", "◵", "◶", "◷"}

var (
	barStyle  = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("117")).Padding(0, 1)
	hintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	dotConn   = lipgloss.NewStyle().Foreground(lipgloss.Color("220")) // yellow
	dotOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))  // green
	dotDown   = lipgloss.NewStyle().Foreground(lipgloss.Color("203")) // red
)

type connState int

const (
	stConnecting connState = iota
	stConnected
	stDown
)

// Per-session messages carry the epoch (generation) of the chat session they
// were armed for. Update drops any whose epoch != m.epoch — stale messages from
// a torn-down dip (including the close-driven "stream closed" errMsg) must not
// leak into the current session's scrollback or re-arm a wait on a dead channel.
// The direct-chat path uses epoch 0 throughout (a single generation).
type (
	incoming struct {
		data  []byte
		epoch int
	}
	stateMsg struct {
		state connState
		epoch int
	}
	tickMsg struct{}
	errMsg  struct {
		error
		epoch int
	}
	// seedMsg carries the initial /history scrollback (oldest-first frames)
	// plus the pagination cursor, delivered once by runWSText before the WS
	// connects (see ws.go). Rendered without touching peer dots — these are
	// history, not live presence. epoch tags it to its session (see above).
	seedMsg struct {
		frames [][]byte
		next   string
		epoch  int
	}
	// olderMsg carries one older /history page fetched on scroll-back (or a
	// failure flag so the model can re-enable the fetch on the next scroll).
	olderMsg struct {
		frames [][]byte
		next   string
		failed bool
	}
)

type model struct {
	host       string
	myName     string
	state      connState
	spinIdx    int
	msgs       []string          // rendered chat lines (base + reaction badges)
	msgBases   []string          // base rendered string for each msgs entry (no badges)
	msgIDs     []string          // server-assigned crockford ID per msgs entry ("" if none)
	idxByMsgID map[string]int    // crockford id → index in msgs/msgBases
	reactions  map[string]map[string][]string // msgID → emoji → []reactorName
	seenColors map[int]time.Time // palette idx → lastSeen
	input      textarea.Model
	recv       <-chan []byte
	states     <-chan connState
	send       chan<- []byte
	w, h       int
	welcome    welcomeState
	// scroll-back state (see PgUp/PgDn handling + the /history pager)
	scrollOff   int            // visual rows scrolled up from the bottom (0 = newest pinned)
	histBase    string         // channel HTTP origin, e.g. https://<id>.botbus.ai
	oldestID    string         // pagination cursor; "" = unknown / start of buffer
	histLoading bool           // a scroll-back /history fetch is in flight
	noMoreHist  bool           // reached the start of the buffer
	seed        <-chan seedMsg // one-shot initial /history scrollback + cursor
	// Console mode: when rooted via newConsoleModel the model carries a roster
	// sub-view and toggles between it and the chat view. startChat/stopChat are
	// injectable lifecycle hooks. startChat dials a live WS for the selected
	// agent's inbox channel and returns the fresh transport channels + display
	// name to rebind onto the model; stopChat cancels that WS. Both are nil in
	// the direct single-channel chat path from newModel.
	mode      viewMode
	roster    rosterModel
	startChat func(channel string) chatSession // opens a live WS to channel; returns the bindable session
	stopChat  func()                           // cancels the active chat WS
	// Onboard flow (the `o` key in roster mode): a tiny two-step inline prompt
	// (name → focus) that mints a child agent under the root and prints its
	// connect URL. onboard is the injected action (real impl wired in runConsole;
	// nil in the direct-chat path). onboardState advances name → focus → done.
	onboard      func(name, focus string) (daemon.ConnectInstructions, error)
	onboardState onboardStep
	onboardName  string
	onboardMsg   string // connect URL, remove result, or error shown under the roster
	// Remove flow (the `d` key in roster mode): a one-step y/n confirm that
	// deregisters the selected agent. remove is the injected action (real impl
	// wired in runConsole; nil in the direct-chat path). confirmingDelete gates
	// the y/n capture; deleteTarget is the agent captured when `d` was pressed
	// (so a moving cursor can't retarget the confirm).
	remove           func(node wire.AgentNode) (routerErr error, err error)
	confirmingDelete bool
	deleteTarget     wire.AgentNode
	// epoch is the session generation. It increments on each dip-in (when
	// binding a new chatSession) and is captured into every wait Cmd's closure
	// so Update can drop messages from a prior, torn-down session. The
	// direct-chat path never increments it, so it stays 0 (one generation).
	epoch int
}

type onboardStep int

const (
	onboardOff onboardStep = iota
	onboardAskName
	onboardAskFocus
)

// chatSession is the transport handle for one dip-in: the fresh per-dip channels
// runWSText pumps into, plus the display name + channel host for rendering. It is
// produced by the startChat hook and bound onto the model by Update so the
// model starts consuming the new channels (via waitRecv/waitState/waitSeed). A
// zero value (returned by tests or when no transport is wired) leaves the chat
// channels nil — waitRecv/waitState/waitSeed all tolerate nil/closed channels.
type chatSession struct {
	recv     <-chan []byte
	states   <-chan connState
	send     chan<- []byte
	seed     <-chan seedMsg
	name     string
	host     string
	histBase string
}

// newModel builds the bubbletea model. fresh=true means we just minted this
// channel for the user (botbus run with no channel arg) — the welcome popup
// then uses the "Your new private channel." copy and auto-shows regardless
// of the per-channel welcomed marker. fresh=false means the user joined an
// existing channel; the popup shows once per new-to-this-machine channel,
// gated by ~/.config/botbus/welcomed/<id>.
// newChatInput builds the chat textarea with the speaker's colored-name prompt.
// Shared by newModel (direct chat) and newConsoleModel (console chat dip), so
// both paths get an identical, non-nil input field.
func newChatInput(myName string) textarea.Model {
	// SetPromptFunc renders the user's own colored name on line 0 and an
	// equal-width blank indent on subsequent lines so multi-line input
	// aligns nicely under the first line of text.
	namePrompt := paletteStyle(nameColor(myName)).Render(myName+":") + " "
	indent := strings.Repeat(" ", lipgloss.Width(namePrompt))
	promptWidth := lipgloss.Width(namePrompt)

	ta := textarea.New()
	ta.Placeholder = "type and Enter · shift+enter newline · /me · /dm name · /compact"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.SetWidth(80)
	// Default InsertNewline is bound to Enter; we want Enter to SEND and
	// only Shift+Enter / Alt+Enter / Ctrl+J to insert a newline. Most
	// terminals don't distinguish Shift+Enter from Enter unless they
	// support kitty / CSI u — Alt+Enter and Ctrl+J are reliable fallbacks.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "alt+enter", "ctrl+j"),
	)
	ta.SetPromptFunc(promptWidth, func(line int) string {
		if line == 0 {
			return namePrompt
		}
		return indent
	})
	ta.Focus()
	return ta
}

func newModel(host, histBase, myName string, fresh bool, recv <-chan []byte, states <-chan connState, send chan<- []byte, seed <-chan seedMsg) model {
	ta := newChatInput(myName)
	// Auto-show the welcome popup on fresh mint OR on first visit to a
	// previously-unseen channel. The per-channel marker file gates the
	// second case so re-launching against a known channel doesn't pop the
	// popup again. The fresh flag wins over the marker so a fresh-mint
	// always shows even if (somehow) the channel ID collided with one we
	// previously welcomed.
	channelID := channelIDFromHost(host)
	autoShow := fresh || !isWelcomed(channelID)
	return model{
		host: host, histBase: histBase, myName: myName, state: stConnecting, input: ta,
		recv: recv, states: states, send: send, seed: seed,
		seenColors: map[int]time.Time{},
		idxByMsgID: map[string]int{},
		reactions:  map[string]map[string][]string{},
		welcome:    welcomeState{visible: autoShow, fresh: fresh},
		// Direct single-channel chat (botbus <channel>): run in chat mode so the
		// mode zero-value (modeRoster) doesn't divert the existing entrypoint
		// into a roster view. The console roots in roster mode via newConsoleModel.
		mode: modeChat,
	}
}

// appendSpoken renders an incoming or outgoing line into the scrollback,
// dispatching /me and /dm to renderSlash. color is the speaker's palette
// index; raw is the full "name: body [id xxx]" wire string. The ID suffix
// is extracted, stripped from display, and tracked so reactions can update
// the rendered line in-place.
func (m *model) appendSpoken(name, body, raw string, id string) {
	color := nameColor(name)
	var base string
	if rendered, ok := renderSlash(name, body, color); ok {
		base = rendered
	} else {
		// Strip ID suffix from the raw string for display.
		display := stripMsgIDString(raw)
		base = paletteStyle(color).Render(display)
	}
	idx := len(m.msgs)
	m.msgs = append(m.msgs, base)
	m.msgBases = append(m.msgBases, base)
	m.msgIDs = append(m.msgIDs, id)
	if id != "" {
		m.idxByMsgID[id] = idx
	}
}

// applyReaction updates the reaction state for a given message ID and
// re-renders that message line with the current reaction badges.
func (m *model) applyReaction(reactorName, emoji, refID string) {
	idx, ok := m.idxByMsgID[refID]
	if !ok {
		return // referenced message not in scrollback; silently ignore
	}
	if m.reactions[refID] == nil {
		m.reactions[refID] = map[string][]string{}
	}
	names := m.reactions[refID][emoji]
	// Toggle: remove if already reacted, add otherwise.
	found := false
	for i, n := range names {
		if n == reactorName {
			m.reactions[refID][emoji] = append(names[:i], names[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		m.reactions[refID][emoji] = append(m.reactions[refID][emoji], reactorName)
	}
	// Re-render the message line with updated badges.
	m.msgs[idx] = m.msgBases[idx] + renderReactionBadges(m.reactions[refID])
}

// renderReactionBadges formats a map of emoji→[]names as a compact badge
// string appended to the message line. Empty map returns "".
func renderReactionBadges(r map[string][]string) string {
	if len(r) == 0 {
		return ""
	}
	// Collect and sort emoji for stable output.
	emojis := make([]string, 0, len(r))
	for e := range r {
		if len(r[e]) > 0 {
			emojis = append(emojis, e)
		}
	}
	sort.Strings(emojis)
	var b strings.Builder
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	for _, e := range emojis {
		names := r[e]
		if len(names) == 0 {
			continue
		}
		b.WriteString(" ")
		b.WriteString(style.Render(fmt.Sprintf("%s×%d", e, len(names))))
	}
	return b.String()
}

// renderHistFrame renders one raw history frame into a scrollback string
// (slash-aware), WITHOUT touching peer dots — history isn't live presence.
// The " [id xxx]" suffix is stripped from display.
func renderHistFrame(raw []byte) string {
	name, body, _, named := parseMsgWithID(raw)
	if !named {
		return stripMsgIDString(string(raw))
	}
	color := nameColor(name)
	if r, ok := renderSlash(name, body, color); ok {
		return r
	}
	return paletteStyle(color).Render(stripMsgIDString(string(raw)))
}

// effWH returns the render width/height with the same fallbacks View uses, so
// scroll clamping in Update matches what View draws.
func (m model) effWH() (w, h int) {
	w, h = m.w, m.h
	if w < 20 {
		w = 80
	}
	if h < 4 {
		h = 24
	}
	return
}

// wrappedRows flattens m.msgs into visual rows soft-wrapped at width w.
func (m model) wrappedRows(w int) []string {
	wrapStyle := lipgloss.NewStyle().Width(w)
	var rows []string
	for _, msg := range m.msgs {
		rows = append(rows, strings.Split(wrapStyle.Render(msg), "\n")...)
	}
	return rows
}

// rowsOf is how many visual rows one message occupies at width w.
func rowsOf(msg string, w int) int {
	return strings.Count(lipgloss.NewStyle().Width(w).Render(msg), "\n") + 1
}

// msgAreaRows is the scrollback row capacity (height minus bar, input, spacer).
// Mirrors View's maxLines so clamping and rendering agree.
func (m model) msgAreaRows() int {
	_, h := m.effWH()
	inputLines := strings.Count(m.input.View(), "\n") + 1
	n := h - 1 - inputLines - 1
	if n < 1 {
		n = 1
	}
	return n
}

// maxScroll is the largest valid scrollOff (rows hidden above the screenful).
func (m model) maxScroll() int {
	w, _ := m.effWH()
	n := len(m.wrappedRows(w)) - m.msgAreaRows()
	if n < 0 {
		n = 0
	}
	return n
}

// scrollBy moves the scrollback ~one page (dir +1 up / -1 down). Scrolling up
// while already at the top triggers a /history fetch for older messages.
func (m model) scrollBy(dir int) (tea.Model, tea.Cmd) {
	ms := m.maxScroll()
	page := m.msgAreaRows() - 1
	if page < 1 {
		page = 1
	}
	if dir > 0 {
		if m.scrollOff >= ms {
			return m.maybeLoadOlder()
		}
		m.scrollOff += page
		if m.scrollOff > ms {
			m.scrollOff = ms
		}
		return m, nil
	}
	m.scrollOff -= page
	if m.scrollOff < 0 {
		m.scrollOff = 0
	}
	return m, nil
}

// maybeLoadOlder starts a scroll-back fetch if one isn't running and there's
// more history (a known cursor, not yet at the buffer start).
func (m model) maybeLoadOlder() (tea.Model, tea.Cmd) {
	if m.histLoading || m.noMoreHist || m.oldestID == "" || m.histBase == "" {
		return m, nil
	}
	m.histLoading = true
	return m, loadOlder(m.histBase, m.oldestID)
}

// waitRecv/waitState/waitSeed each capture the epoch they were armed for so the
// resulting msg is tagged to its session. Update drops msgs whose epoch doesn't
// match the model's current epoch (a torn-down dip) and only re-arms waits for
// the current epoch — a stale generation's channels close and its goroutine
// exits rather than spinning. A closed channel yields a zero value (the `_` ok),
// which the epoch guard then drops for any stale session.
func waitRecv(c <-chan []byte, epoch int) tea.Cmd {
	return func() tea.Msg {
		m, ok := <-c
		if !ok {
			return errMsg{fmt.Errorf("stream closed"), epoch}
		}
		return incoming{data: m, epoch: epoch}
	}
}
func waitState(c <-chan connState, epoch int) tea.Cmd {
	return func() tea.Msg { s, _ := <-c; return stateMsg{state: s, epoch: epoch} }
}
func tickCmd() tea.Cmd {
	return tea.Tick(spinSpeed, func(time.Time) tea.Msg { return tickMsg{} })
}

// waitSeed blocks for the one-shot initial /history scrollback delivered by
// runWSText (oldest-first frames + pagination cursor). A closed channel yields
// an empty seed so the model still proceeds. Not re-armed — one-shot. The epoch
// is stamped here (overriding whatever ws.go sent) so a stale dip's seed is
// dropped by the guard in Update.
func waitSeed(c <-chan seedMsg, epoch int) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return seedMsg{epoch: epoch}
		}
		s, ok := <-c
		if !ok {
			return seedMsg{epoch: epoch}
		}
		s.epoch = epoch
		return s
	}
}

// loadOlder fetches one older /history page strictly before `before`. The
// result (olderMsg) is applied in Update; a fetch error sets failed so the
// model re-enables the fetch on the next scroll.
func loadOlder(base, before string) tea.Cmd {
	return func() tea.Msg {
		p, err := fetchHistory(context.Background(), base, "/history", before, 40)
		if err != nil {
			return olderMsg{failed: true}
		}
		return olderMsg{frames: histFramesChrono(p), next: p.Next}
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(waitRecv(m.recv, m.epoch), waitState(m.states, m.epoch), waitSeed(m.seed, m.epoch), tea.WindowSize(), tickCmd())
}

func (m model) publish(text string) tea.Cmd {
	send := m.send
	epoch := m.epoch
	out := []byte(m.myName + ": " + text)
	return func() tea.Msg {
		select {
		case send <- out:
			return nil
		case <-time.After(5 * time.Second):
			return errMsg{fmt.Errorf("send timeout"), epoch}
		}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Console mode switch (Task 5). In roster mode the roster sub-view owns
	// navigation; enter dips into the selected agent's inbox channel (starting
	// the chat via the injected hook) and switches to chat mode. esc quits.
	// In chat mode, esc tears the chat down and returns to the roster instead
	// of quitting; everything else falls through to the chat Update below.
	if m.mode == modeRoster {
		// Onboard prompt active: capture name → focus inline, then mint the
		// child and show the connect URL. esc aborts back to plain roster.
		if m.onboardState != onboardOff {
			return m.updateOnboard(msg)
		}
		// Remove confirm active: capture y/n then act.
		if m.confirmingDelete {
			if k, ok := msg.(tea.KeyMsg); ok {
				switch k.String() {
				case "y", "Y":
					routerErr, err := m.remove(m.deleteTarget)
					if err != nil {
						m.onboardMsg = "can't remove " + m.deleteTarget.Name + ": " + err.Error()
					} else {
						m.roster.removeByID(m.deleteTarget.ID)
						m.onboardMsg = "removed " + m.deleteTarget.Name
						if routerErr != nil {
							m.onboardMsg += " (locally; router: " + routerErr.Error() + ")"
						}
					}
					m.confirmingDelete = false
					return m, nil
				case "n", "N", "esc", "ctrl+c":
					m.confirmingDelete = false
					m.onboardMsg = ""
					return m, nil
				}
				return m, nil // swallow other keys during confirm
			}
		}
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.String() {
			case "esc", "ctrl+c":
				return m, tea.Quit
			case "o":
				// Begin onboarding only when a real action is wired (console mode).
				if m.onboard != nil {
					m.onboardState = onboardAskName
					m.onboardName = ""
					m.onboardMsg = ""
				}
				return m, nil
			case "d":
				// Begin a remove confirm only when a real action is wired (console
				// mode) and there's a selected agent to remove.
				if m.remove != nil {
					sel := m.roster.selected()
					if sel.InboxChannel != "" || sel.ID != "" {
						m.confirmingDelete = true
						m.deleteTarget = sel
						m.onboardMsg = ""
					}
				}
				return m, nil
			}
		}
		r, dip := m.roster.updateRoster(msg)
		m.roster = r
		if dip {
			sel := m.roster.selected()
			// Only enter chat mode when there's actually a session to start —
			// a node with an empty InboxChannel (or no startChat hook) would
			// otherwise strand the user in an empty, un-dialable chat view.
			if sel.InboxChannel != "" && m.startChat != nil {
				m.mode = modeChat
				// Open the live WS to the selected agent's inbox and rebind the
				// model onto the fresh transport channels, then start consuming
				// them — mirroring how main() issues the initial waits for the
				// direct-chat path. A zero-value session (no transport wired,
				// e.g. in tests) leaves the channels nil and skips the waits.
				s := m.startChat(sel.InboxChannel)
				if s.recv != nil {
					m.recv = s.recv
					m.states = s.states
					m.send = s.send
					m.seed = s.seed
					m.host = s.host
					m.histBase = s.histBase
					if s.name != "" {
						m.myName = s.name
						m.input = newChatInput(s.name)
					}
					m.state = stConnecting
					// New generation: bump the epoch and arm waits tagged with it.
					// Any messages still in flight from the prior dip carry the old
					// epoch and get dropped by the guards in Update.
					m.epoch++
					return m, tea.Batch(waitRecv(m.recv, m.epoch), waitState(m.states, m.epoch), waitSeed(m.seed, m.epoch), tickCmd())
				}
			}
		}
		return m, nil
	}
	// mode == modeChat: esc returns to the roster instead of quitting (ctrl+c
	// still quits, handled in the chat key switch below). Only meaningful when
	// the model was rooted as a console — a direct chat model has no roster to
	// return to, but its esc-quits behavior is unchanged because such a model
	// is never in roster mode and its roster is empty (selecting nothing).
	if k, ok := msg.(tea.KeyMsg); ok && !m.welcome.visible && k.String() == "esc" && m.stopChat != nil {
		m.stopChat()
		m.mode = modeRoster
		// Clear the chat scrollback so the next dip starts fresh.
		m.msgs = nil
		m.msgBases = nil
		m.msgIDs = nil
		m.idxByMsgID = map[string]int{}
		m.reactions = map[string]map[string][]string{}
		m.scrollOff = 0
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		w := msg.Width - lipgloss.Width(quitHint) - 4
		if w < 8 {
			w = 8
		}
		m.input.SetWidth(w)
		// Re-clamp the scroll offset to the new size so a shrink doesn't leave
		// it stale-high (which would make the next PgUp fetch history instead
		// of scrolling).
		if ms := m.maxScroll(); m.scrollOff > ms {
			m.scrollOff = ms
		}
		return m, nil
	case tea.KeyMsg:
		// Welcome popup intercepts most keys while visible. Ctrl-C still
		// quits (universal escape hatch); Ctrl-H, Esc, and Enter dismiss.
		// Other keys are swallowed so the user doesn't accidentally start
		// typing chat into a hidden input field.
		if m.welcome.visible {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "ctrl+h", "esc", "enter":
				m.welcome.visible = false
				// Persist that we've shown this user the welcome for this
				// channel; ignore errors (re-popping next time is harmless).
				_ = markWelcomed(channelIDFromHost(m.host))
				return m, nil
			}
			// Everything else: do nothing, but DON'T fall through to the
			// textarea update at the bottom of this function — we want the
			// chat input inert while the popup is up.
			return m, nil
		}
		// Ctrl-H re-summons the popup at any time (preserves the fresh
		// variant if this run was a fresh mint).
		if msg.String() == "ctrl+h" {
			m.welcome.visible = true
			return m, nil
		}
		switch msg.String() {
		case "esc", "ctrl+c":
			return m, tea.Quit
		case "pgup":
			return m.scrollBy(+1) // scroll into history; fetches older at the top
		case "pgdown":
			return m.scrollBy(-1)
		case "home":
			m.scrollOff = m.maxScroll()
			return m.maybeLoadOlder()
		case "end":
			m.scrollOff = 0 // jump back to the newest
			return m, nil
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()
			m.input.SetHeight(1)
			// Show our own line locally exactly as peers will see it
			// (slash-aware rendering), and snap to the bottom so we always
			// see what we just sent even if we were scrolled up in history.
			// Our own messages don't have server-assigned IDs yet (they
			// arrive back as broadcasts with IDs, but we don't echo to self).
			m.appendSpoken(m.myName, text, m.myName+": "+text, "")
			m.scrollOff = 0
			return m, m.publish(text)
		}
	case incoming:
		// Drop frames from a torn-down dip: don't append, don't re-arm a wait on
		// the stale (now closed) channel — let that generation die.
		if msg.epoch != m.epoch {
			return m, nil
		}
		name, body, id, named := parseMsgWithID(msg.data)
		text := string(msg.data)
		prevLen := len(m.msgs)
		if named {
			m.seenColors[nameColor(name)] = time.Now()
			// Intercept reaction messages: render as badge on the referenced
			// message rather than as a new chat line.
			if emoji, refID, isReaction := parseReaction(body); isReaction {
				m.applyReaction(name, emoji, refID)
			} else {
				m.appendSpoken(name, body, text, id)
			}
		} else {
			m.msgs = append(m.msgs, stripMsgIDString(text))
			m.msgBases = append(m.msgBases, stripMsgIDString(text))
			m.msgIDs = append(m.msgIDs, id)
		}
		// If the user is scrolled up reading history, keep their view anchored
		// (grow the offset by the new line's rows) rather than yanking them to
		// the bottom. At the bottom (scrollOff==0) the new line just shows.
		// Only adjust if a new line was actually appended (reactions don't add lines).
		if m.scrollOff > 0 && len(m.msgs) > prevLen {
			w, _ := m.effWH()
			m.scrollOff += rowsOf(m.msgs[len(m.msgs)-1], w)
		}
		return m, waitRecv(m.recv, m.epoch)
	case seedMsg:
		// Drop a stale dip's seed (it carries the old generation's history).
		if msg.epoch != m.epoch {
			return m, nil
		}
		// One-shot initial /history scrollback: append oldest-first (these are
		// the newest ~40, in order) and set the pagination cursor. Stays pinned
		// to the bottom (scrollOff already 0). Not re-armed.
		for _, f := range msg.frames {
			m.msgs = append(m.msgs, renderHistFrame(f))
		}
		if msg.next != "" {
			m.oldestID = msg.next
		} else if len(msg.frames) > 0 {
			m.noMoreHist = true // seeded the whole buffer; nothing older
		}
		return m, nil
	case olderMsg:
		m.histLoading = false
		if msg.failed {
			return m, nil // transient — a later scroll retries
		}
		if len(msg.frames) == 0 {
			m.noMoreHist = true
			return m, nil
		}
		w, _ := m.effWH()
		rendered := make([]string, 0, len(msg.frames))
		added := 0
		for _, f := range msg.frames {
			s := renderHistFrame(f)
			rendered = append(rendered, s)
			added += rowsOf(s, w)
		}
		// Prepend older messages; re-index idxByMsgID to account for the shift.
		offset := len(rendered)
		for id, idx := range m.idxByMsgID {
			m.idxByMsgID[id] = idx + offset
		}
		bases := make([]string, len(rendered))
		copy(bases, rendered)
		ids := make([]string, len(rendered)) // history frames don't carry live IDs for reaction lookup
		m.msgs = append(rendered, m.msgs...)
		m.msgBases = append(bases, m.msgBases...)
		m.msgIDs = append(ids, m.msgIDs...)
		m.scrollOff += added // keep the viewport anchored on the same content
		if msg.next != "" {
			m.oldestID = msg.next
		} else {
			m.noMoreHist = true
		}
		return m, nil
	case stateMsg:
		// Stale dip's connection state: ignore (and don't re-arm the dead chan).
		if msg.epoch != m.epoch {
			return m, nil
		}
		m.state = msg.state
		return m, waitState(m.states, m.epoch)
	case tickMsg:
		m.spinIdx++
		return m, tickCmd()
	case errMsg:
		// The close-driven "stream closed" from a torn-down dip arrives here with
		// the old epoch — drop it so it doesn't pollute the new session's view.
		if msg.epoch != m.epoch {
			return m, nil
		}
		m.msgs = append(m.msgs, errStyle.Render("! "+msg.Error()))
		return m, nil
	}
	// textarea's internal repositionView() scrolls the cursor into the
	// current viewport.Height during Update — if Height is smaller than the
	// new cursor row (e.g. just after shift+enter or a soft-wrap), the top
	// of the input gets hidden by viewport YOffset. Pre-set Height to the
	// max we'd ever want so the cursor always stays in view during the
	// keypress, then trim to the actual visual content size after.
	m.input.SetHeight(maxInputRows)
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	target := visualRows(m.input.Value(), m.input.Width())
	if target > maxInputRows {
		target = maxInputRows
	}
	if target < 1 {
		target = 1
	}
	m.input.SetHeight(target)
	return m, cmd
}

// renderDots: peer dots (one per recently-seen palette color, excluding
// self). Sorted by palette index so order is stable across renders — Go
// map iteration is randomized and was visibly flickering the bar.
//
// Status dot appears only when no peers are visible OR we're not fully
// connected — peer dots already imply liveness.
func (m model) renderDots() string {
	var dots []string
	cutoff := time.Now().Add(-dotTTL)
	myColor := nameColor(m.myName)
	var live []int
	for c, t := range m.seenColors {
		if t.After(cutoff) && c != myColor {
			live = append(live, c)
		}
	}
	sort.Ints(live)
	for _, c := range live {
		dots = append(dots, paletteStyle(c).Render("●"))
	}
	if len(dots) == 0 || m.state != stConnected {
		switch m.state {
		case stConnected:
			dots = append(dots, dotOK.Render("●"))
		case stDown:
			dots = append(dots, dotDown.Render("●"))
		default:
			dots = append(dots, dotConn.Render(spinFrames[m.spinIdx%len(spinFrames)]))
		}
	}
	return strings.Join(dots, " ")
}

func (m model) View() string {
	// Console roster mode renders the agent tree; chat mode falls through to
	// the existing chat view below.
	if m.mode == modeRoster {
		out := m.roster.View()
		// Inline onboarding prompt: append the current step's input line.
		switch m.onboardState {
		case onboardAskName:
			out += "\n\n" + hintStyle.Render("onboard — new agent name:") + "\n" + m.input.View()
		case onboardAskFocus:
			out += "\n\n" + hintStyle.Render("onboard "+m.onboardName+" — focus area:") + "\n" + m.input.View()
		}
		// Remove confirm prompt.
		if m.confirmingDelete {
			out += "\n\n" + hintStyle.Render("remove "+m.deleteTarget.Name+"? (y/n)")
		}
		if m.onboardMsg != "" {
			out += "\n\n" + hintStyle.Render(m.onboardMsg)
		}
		return out
	}
	h, w := m.h, m.w
	if h < 4 {
		h = 24
	}
	if w < 20 {
		w = 80
	}
	var b strings.Builder

	// Welcome popup overlay: render the top bar so the user can still see
	// the connection state, then fill the rest of the viewport with the
	// centered popup. Input is hidden — the popup's footer line tells the
	// user how to dismiss.
	if m.welcome.visible {
		left, right := "BOTBUS · "+m.host, m.renderDots()
		pad := w - 2 - lipgloss.Width(left) - lipgloss.Width(right)
		if pad < 1 {
			pad = 1
		}
		b.WriteString(barStyle.Render(left + strings.Repeat(" ", pad) + right))
		b.WriteByte('\n')
		channelID := channelIDFromHost(m.host)
		popup := renderWelcomePopup(channelID, m.welcome.fresh, w)
		// Center vertically in the remaining h-1 rows.
		placed := lipgloss.Place(w, h-1, lipgloss.Center, lipgloss.Center, popup)
		b.WriteString(placed)
		return b.String()
	}

	left, right := "BOTBUS · "+m.host, m.renderDots()
	pad := w - 2 - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	b.WriteString(barStyle.Render(left + strings.Repeat(" ", pad) + right))
	b.WriteByte('\n')

	// Multi-line input grows from the bottom up; reserve rows accordingly.
	inputView := m.input.View()
	inputLines := strings.Count(inputView, "\n") + 1
	maxLines := h - 1 - inputLines - 1 // -1 bar, -inputLines, -1 spacer
	if maxLines < 1 {
		maxLines = 1
	}

	// Wrap scrollback messages so long lines don't get truncated at the
	// terminal edge; collect from newest backwards until we've filled
	// maxLines worth of visual rows. lipgloss.Width(w).Render handles
	// ANSI-aware soft-wrapping at the right cell column.
	// Flatten messages to visual rows, then show a maxLines-tall window whose
	// bottom sits scrollOff rows above the newest. scrollOff==0 pins to the
	// latest (the default); PgUp raises it to reveal history.
	wrapStyle := lipgloss.NewStyle().Width(w)
	var rows []string
	for _, msg := range m.msgs {
		rows = append(rows, strings.Split(wrapStyle.Render(msg), "\n")...)
	}
	off := m.scrollOff
	maxOff := len(rows) - maxLines
	if maxOff < 0 {
		maxOff = 0
	}
	if off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	end := len(rows) - off
	start := end - maxLines
	if start < 0 {
		start = 0
	}
	window := rows[start:end]
	for i := len(window); i < maxLines; i++ {
		b.WriteByte('\n')
	}
	for _, line := range window {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	// Hint sits on the first line of the input, to the right. While scrolled
	// up, swap in a scrollback hint so the user knows how to get back to live.
	hintText := quitHint
	if m.scrollOff > 0 {
		hintText = "↑ history · End→latest"
	}
	lines := strings.Split(inputView, "\n")
	hint := hintStyle.Render(hintText)
	ipad := w - lipgloss.Width(lines[0]) - lipgloss.Width(hint)
	if ipad < 1 {
		ipad = 1
	}
	b.WriteString(lines[0] + strings.Repeat(" ", ipad) + hint)
	for _, line := range lines[1:] {
		b.WriteByte('\n')
		b.WriteString(line)
	}
	return b.String()
}
