package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
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
	dotTTL    = 5 * time.Minute
	spinSpeed = 150 * time.Millisecond
	quitHint  = "Esc to quit"
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
	input      textinput.Model
	recv       <-chan []byte
	states     <-chan connState
	send       chan<- []byte
	w, h       int
}

func newModel(host, myName string, recv <-chan []byte, states <-chan connState, send chan<- []byte) model {
	ti := textinput.New()
	ti.Placeholder = "type and Enter · this URL is the secret"
	// Prompt shows the user's own name in their color, with a trailing ": "
	// — matches what others will see when receiving the message.
	ti.Prompt = paletteStyle(nameColor(myName)).Render(myName+":") + " "
	ti.Focus()
	return model{
		host: host, myName: myName, state: stConnecting, input: ti,
		recv: recv, states: states, send: send,
		seenColors: map[int]time.Time{},
	}
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
		m.input.Width = msg.Width - len(quitHint) - 4
		if m.input.Width < 8 {
			m.input.Width = 8
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.SetValue("")
			// Show our own line exactly as peers will see it.
			full := m.myName + ": " + text
			m.msgs = append(m.msgs, paletteStyle(nameColor(m.myName)).Render(full))
			return m, m.publish(text)
		}
	case incoming:
		name, _, named := parseMsg(msg)
		text := string(msg)
		if named {
			color := nameColor(name)
			m.seenColors[color] = time.Now()
			m.msgs = append(m.msgs, paletteStyle(color).Render(text))
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
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
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

	// Bottom-up: pad blank lines before messages so newest sits at the input.
	maxLines := h - 3
	if maxLines < 1 {
		maxLines = 1
	}
	start := 0
	if len(m.msgs) > maxLines {
		start = len(m.msgs) - maxLines
	}
	shown := m.msgs[start:]
	for i := len(shown); i < maxLines; i++ {
		b.WriteByte('\n')
	}
	for _, line := range shown {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	inputView := m.input.View()
	hint := hintStyle.Render(quitHint)
	ipad := w - lipgloss.Width(inputView) - lipgloss.Width(hint)
	if ipad < 1 {
		ipad = 1
	}
	b.WriteString(inputView + strings.Repeat(" ", ipad) + hint)
	return b.String()
}
