package main

// console.go — the roster screen sub-model for the hierarchical console: the
// operator's at-a-glance view of their agent tree (every agent, ● live / ○
// idle, name, focus) plus cursor navigation. The main TUI embeds this and owns
// mode switching + dip-in (Task 5); here we only do roster data + navigation +
// View rendering. No WebSocket, no mode switching.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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
	b.WriteString("\n" + hintStyle.Render("↑/↓ navigate · enter dip in · esc quit"))
	return b.String()
}
