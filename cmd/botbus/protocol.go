package main

// protocol.go — wire protocol helpers: name/body parsing, name-derived colors,
// the styled palette, slash-command rendering, and the small visual-rows math
// used to size the input area. All of this is pure rendering logic shared by
// both the interactive TUI (ui.go) and monitor mode's audio drain (main.go).
//
// The palette and nameColor implementation MUST match web/channel.html on the
// server — the same name renders in the same color in the browser, CLI, and
// any other client.

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// palette: 32 high-contrast hues. Indexed by nameColor(name).
// MUST match the PAL array in web/channel.html.
var palette = []string{
	"#f87171", "#fb923c", "#fbbf24", "#facc15",
	"#a3e635", "#4ade80", "#34d399", "#2dd4bf",
	"#22d3ee", "#38bdf8", "#60a5fa", "#a855f7",
	"#e879f9", "#f43f5e", "#f472b6", "#fb7185",
	"#ef4444", "#f97316", "#eab308", "#84cc16",
	"#22c55e", "#14b8a6", "#06b6d4", "#3b82f6",
	"#8b5cf6", "#ec4899", "#6366f1", "#10b981",
	"#f59e0b", "#d946ef", "#0ea5e9", "#a16207",
}

func paletteStyle(i int) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(palette[i&0x1F]))
}

// nameColor: sum of Unicode codepoints mod 32. Trivial, deterministic, and
// cheap to mirror in JS (channel.html has the identical algorithm).
func nameColor(name string) int {
	sum := 0
	for _, r := range name {
		sum += int(r)
	}
	return sum & 0x1F
}

// parseMsg splits "name: body" into (name, body, true). Bytes without that
// shape return ("", text, false). The " [id xxx]" suffix (if present) is
// stripped from body before parsing — callers that need the ID should use
// parseMsgWithID instead.
func parseMsg(b []byte) (name, body string, named bool) {
	name, body, _, named = parseMsgWithID(b)
	return
}

// parseMsgWithID splits "name: body [id xxx]" into (name, body, id, true).
// The id is the Crockford base32 token from the server-stamped suffix, or ""
// if absent. Frames without a "name: " prefix return ("", text, "", false).
func parseMsgWithID(b []byte) (name, body, id string, named bool) {
	s := stripMsgIDString(string(b))
	rawID := extractMsgID(string(b))
	if i := strings.Index(s, ": "); i > 0 {
		return s[:i], s[i+2:], rawID, true
	}
	return "", s, rawID, false
}

// extractMsgID returns the Crockford base32 token from a " [id xxx]" suffix,
// or "" if not present.
func extractMsgID(s string) string {
	i := strings.LastIndex(s, " [id ")
	if i < 0 || s[len(s)-1] != ']' {
		return ""
	}
	return s[i+5 : len(s)-1]
}

// stripMsgIDString removes the " [id xxx]" suffix from a string.
func stripMsgIDString(s string) string {
	i := strings.LastIndex(s, " [id ")
	if i < 0 || s[len(s)-1] != ']' {
		return s
	}
	return s[:i]
}

// parseReaction checks if body (from a "name: body" message) is a reaction:
// "reacted <emoji> to <id>". Returns (emoji, refID, true) on match.
func parseReaction(body string) (emoji, refID string, ok bool) {
	rest, cut := strings.CutPrefix(body, "reacted ")
	if !cut {
		return "", "", false
	}
	// rest = "<emoji> to <id>"
	toIdx := strings.LastIndex(rest, " to ")
	if toIdx < 0 {
		return "", "", false
	}
	emoji = strings.TrimSpace(rest[:toIdx])
	refID = strings.TrimSpace(rest[toIdx+4:])
	if emoji == "" || refID == "" {
		return "", "", false
	}
	return emoji, refID, true
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
	if body == "/compact" {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#525252")).Italic(true).Render("⟳ " + name + " requested /compact"), true
	}
	return "", false
}
