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
)

const (
	dotTTL       = 5 * time.Minute
	spinSpeed    = 150 * time.Millisecond
	quitHint     = "Esc quit · PgUp history · /me · /dm"
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

type (
	incoming []byte
	stateMsg connState
	tickMsg  struct{}
	errMsg   struct{ error }
	// seedMsg carries the initial /history scrollback (oldest-first frames)
	// plus the pagination cursor, delivered once by runWSText before the WS
	// connects (see ws.go). Rendered without touching peer dots — these are
	// history, not live presence.
	seedMsg struct {
		frames [][]byte
		next   string
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
	msgs       []string
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
}

// newModel builds the bubbletea model. fresh=true means we just minted this
// channel for the user (botbus run with no channel arg) — the welcome popup
// then uses the "Your new private channel." copy and auto-shows regardless
// of the per-channel welcomed marker. fresh=false means the user joined an
// existing channel; the popup shows once per new-to-this-machine channel,
// gated by ~/.config/botbus/welcomed/<id>.
func newModel(host, histBase, myName string, fresh bool, recv <-chan []byte, states <-chan connState, send chan<- []byte, seed <-chan seedMsg) model {
	// SetPromptFunc renders the user's own colored name on line 0 and an
	// equal-width blank indent on subsequent lines so multi-line input
	// aligns nicely under the first line of text.
	namePrompt := paletteStyle(nameColor(myName)).Render(myName+":") + " "
	indent := strings.Repeat(" ", lipgloss.Width(namePrompt))
	promptWidth := lipgloss.Width(namePrompt)

	ta := textarea.New()
	ta.Placeholder = "type and Enter · shift+enter newline · /me · /dm name"
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
		welcome:    welcomeState{visible: autoShow, fresh: fresh},
	}
}

// appendSpoken renders an incoming or outgoing line into the scrollback,
// dispatching /me and /dm to renderSlash. color is the speaker's palette
// index; raw is the full "name: body" string used as the plain fallback.
func (m *model) appendSpoken(name, body, raw string) {
	color := nameColor(name)
	if rendered, ok := renderSlash(name, body, color); ok {
		m.msgs = append(m.msgs, rendered)
		return
	}
	m.msgs = append(m.msgs, paletteStyle(color).Render(raw))
}

// renderHistFrame renders one raw history frame into a scrollback string
// (slash-aware), WITHOUT touching peer dots — history isn't live presence.
func renderHistFrame(raw []byte) string {
	name, body, named := parseMsg(raw)
	if !named {
		return string(raw)
	}
	color := nameColor(name)
	if r, ok := renderSlash(name, body, color); ok {
		return r
	}
	return paletteStyle(color).Render(string(raw))
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

func waitRecv(c <-chan []byte) tea.Cmd {
	return func() tea.Msg {
		m, ok := <-c
		if !ok {
			return errMsg{fmt.Errorf("stream closed")}
		}
		return incoming(m)
	}
}
func waitState(c <-chan connState) tea.Cmd {
	return func() tea.Msg { s, _ := <-c; return stateMsg(s) }
}
func tickCmd() tea.Cmd {
	return tea.Tick(spinSpeed, func(time.Time) tea.Msg { return tickMsg{} })
}

// waitSeed blocks for the one-shot initial /history scrollback delivered by
// runWSText (oldest-first frames + pagination cursor). A closed channel
// yields an empty seed so the model still proceeds. Not re-armed — one-shot.
func waitSeed(c <-chan seedMsg) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return seedMsg{}
		}
		s, ok := <-c
		if !ok {
			return seedMsg{}
		}
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
	return tea.Batch(waitRecv(m.recv), waitState(m.states), waitSeed(m.seed), tea.WindowSize(), tickCmd())
}

func (m model) publish(text string) tea.Cmd {
	send := m.send
	out := []byte(m.myName + ": " + text)
	return func() tea.Msg {
		select {
		case send <- out:
			return nil
		case <-time.After(5 * time.Second):
			return errMsg{fmt.Errorf("send timeout")}
		}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		w := msg.Width - lipgloss.Width(quitHint) - 4
		if w < 8 {
			w = 8
		}
		m.input.SetWidth(w)
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
			m.appendSpoken(m.myName, text, m.myName+": "+text)
			m.scrollOff = 0
			return m, m.publish(text)
		}
	case incoming:
		name, body, named := parseMsg(msg)
		text := string(msg)
		if named {
			m.seenColors[nameColor(name)] = time.Now()
			m.appendSpoken(name, body, text)
		} else {
			m.msgs = append(m.msgs, text)
		}
		// If the user is scrolled up reading history, keep their view anchored
		// (grow the offset by the new line's rows) rather than yanking them to
		// the bottom. At the bottom (scrollOff==0) the new line just shows.
		if m.scrollOff > 0 {
			w, _ := m.effWH()
			m.scrollOff += rowsOf(m.msgs[len(m.msgs)-1], w)
		}
		return m, waitRecv(m.recv)
	case seedMsg:
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
		m.msgs = append(rendered, m.msgs...) // prepend older messages above
		m.scrollOff += added                 // keep the viewport anchored on the same content
		if msg.next != "" {
			m.oldestID = msg.next
		} else {
			m.noMoreHist = true
		}
		return m, nil
	case stateMsg:
		m.state = connState(msg)
		return m, waitState(m.states)
	case tickMsg:
		m.spinIdx++
		return m, tickCmd()
	case errMsg:
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
