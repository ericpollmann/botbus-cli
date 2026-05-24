package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Protocol: messages are plain UTF-8 text. The convention is "name: body";
// the color of a message is derived from its name via nameColor (sum of
// codepoints mod 16). Plain text without "name: " renders in the default
// color. Curl-friendly — `curl -d 'eric: hi'` works.
//
// Palette MUST match web/channel.html. nameColor MUST match the JS impl
// in channel.html (sum of codepoints, mod 16).
const (
	dotTTL       = 5 * time.Minute
	spinSpeed    = 150 * time.Millisecond
	quitHint     = "Esc to quit · /me · /dm name"
	maxInputRows = 8
)

var palette = []string{
	"#f87171", "#fb923c", "#fbbf24", "#facc15",
	"#a3e635", "#4ade80", "#34d399", "#2dd4bf",
	"#22d3ee", "#38bdf8", "#60a5fa", "#818cf8",
	"#a78bfa", "#c084fc", "#f472b6", "#fb7185",
}
var spinFrames = []string{"◴", "◵", "◶", "◷"}

var (
	barStyle  = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("117")).Padding(0, 1)
	hintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	dotConn   = lipgloss.NewStyle().Foreground(lipgloss.Color("220")) // yellow
	dotOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))  // green
	dotDown   = lipgloss.NewStyle().Foreground(lipgloss.Color("203")) // red
)

func paletteStyle(i int) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(palette[i&0x0F]))
}

// nameColor: sum of Unicode codepoints mod 16. Trivial, deterministic, and
// cheap to mirror in JS (channel.html has the identical algorithm).
func nameColor(name string) int {
	sum := 0
	for _, r := range name {
		sum += int(r)
	}
	return sum & 0x0F
}

// parseMsg splits "name: body" into (name, body, true). Bytes without that
// shape return ("", text, false).
func parseMsg(b []byte) (name, body string, named bool) {
	s := string(b)
	if i := strings.Index(s, ": "); i > 0 {
		return s[:i], s[i+2:], true
	}
	return "", s, false
}

// visualRows reports the total number of rendered terminal rows the given
// content will occupy after soft-wrapping at the given width. Returns at
// least 1 even for empty content. This mirrors how bubbles/textarea wraps
// internally (character-level at width m.width) — we recompute here because
// the textarea's wrap function is unexported and its View() always pads to
// the configured Height so counting "\n" in View output is uninformative.
//
// Used by the Update loop to keep the textarea's height tracking the
// content's visual size so wrapped lines don't get hidden by the internal
// viewport scrolling cursor into view.
func visualRows(value string, width int) int {
	if width <= 0 || value == "" {
		return 1
	}
	total := 0
	for _, line := range strings.Split(value, "\n") {
		vw := lipgloss.Width(line)
		rows := (vw + width - 1) / width
		if rows < 1 {
			rows = 1
		}
		total += rows
	}
	if total < 1 {
		total = 1
	}
	return total
}

// renderSlash returns the styled string for /me and /dm slash commands, or
// ("", false) if body isn't a recognized slash command. Both commands render
// in italic in the speaker's color. /dm is a convention only — the channel
// is fundamentally public; the TARGET is encoded in the body prefix and the
// receiving line just labels who it was directed at.
func renderSlash(name, body string, color int) (string, bool) {
	if action, ok := strings.CutPrefix(body, "/me "); ok {
		return paletteStyle(color).Italic(true).Render("* " + name + " " + action), true
	}
	if rest, ok := strings.CutPrefix(body, "/dm "); ok {
		if sp := strings.Index(rest, " "); sp > 0 {
			target, dmText := rest[:sp], rest[sp+1:]
			return paletteStyle(color).Italic(true).Render(name + " → " + target + ": " + dmText), true
		}
	}
	return "", false
}

// isTypedFrame reports whether a wire frame uses the typed-frame protocol
// (first byte < 0x20). Type 0x01 is audio (the web /voice UI sends these);
// other low bytes are reserved. The CLI is text-only — typed frames are
// silently dropped on receive. Text frames always start with a printable
// ASCII byte (the first character of a name) so this check is unambiguous.
func isTypedFrame(b []byte) bool {
	return len(b) > 0 && b[0] < 0x20
}

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
}

func newModel(host, myName string, recv <-chan []byte, states <-chan connState, send chan<- []byte) model {
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
	return model{
		host: host, myName: myName, state: stConnecting, input: ta,
		recv: recv, states: states, send: send,
		seenColors: map[int]time.Time{},
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

func (m model) Init() tea.Cmd {
	return tea.Batch(waitRecv(m.recv), waitState(m.states), tea.WindowSize(), tickCmd())
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
		switch msg.String() {
		case "esc", "ctrl+c":
			return m, tea.Quit
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()
			m.input.SetHeight(1)
			// Show our own line locally exactly as peers will see it
			// (slash-aware rendering).
			m.appendSpoken(m.myName, text, m.myName+": "+text)
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
		return m, waitRecv(m.recv)
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
	wrapStyle := lipgloss.NewStyle().Width(w)
	var displayed []string
	totalRows := 0
	for i := len(m.msgs) - 1; i >= 0; i-- {
		wrapped := wrapStyle.Render(m.msgs[i])
		rows := strings.Count(wrapped, "\n") + 1
		if totalRows+rows > maxLines {
			break
		}
		displayed = append([]string{wrapped}, displayed...)
		totalRows += rows
	}
	for i := totalRows; i < maxLines; i++ {
		b.WriteByte('\n')
	}
	for _, line := range displayed {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	// Hint sits on the first line of the input, to the right.
	lines := strings.Split(inputView, "\n")
	hint := hintStyle.Render(quitHint)
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
