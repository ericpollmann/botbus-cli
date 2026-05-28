package main

// ui.go — bubbletea model + Init/Update/View for the interactive chat TUI.
// Pure rendering helpers (palette, nameColor, parseMsg, renderSlash,
// visualRows) live in protocol.go.

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

const (
	dotTTL       = 5 * time.Minute
	spinSpeed    = 150 * time.Millisecond
	quitHint     = "Esc to quit · /me · /dm name"
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
}

// newModel builds the bubbletea model. fresh=true means we just minted this
// channel for the user (botbus run with no channel arg) — the welcome popup
// then uses the "Your new private channel." copy and auto-shows regardless
// of the per-channel welcomed marker. fresh=false means the user joined an
// existing channel; the popup shows once per new-to-this-machine channel,
// gated by ~/.config/botbus/welcomed/<id>.
func newModel(host, myName string, fresh bool, recv <-chan []byte, states <-chan connState, send chan<- []byte) model {
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
		host: host, myName: myName, state: stConnecting, input: ta,
		recv: recv, states: states, send: send,
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
