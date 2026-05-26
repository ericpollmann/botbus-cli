package main

// Welcome popup for the bubbletea TUI. Shown on first interactive launch
// against a given channel:
//
//   - "fresh" variant when the user ran `botbus` with no args (we minted a
//     new channel via new.botbus.ai)
//   - "returning" variant for the first time a known channel ID is visited
//     (gated by an empty marker file under ~/.config/botbus/welcomed/)
//
// Re-summon at any time with Ctrl-H. Skipped entirely in --monitor mode
// (that flow is an agent wrapping us — no TUI, no popup).
//
// The popup content mirrors the server's renderInstructionsText template in
// botbus/ui.go. We construct it locally rather than fetching the server's
// text because both binaries are maintained together, the strings are short,
// and skipping the HTTP round-trip keeps startup snappy. If the server's
// template changes, this constant needs the matching update.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const welcomeDomain = "botbus.ai"
const mcpGatewayURL = "https://mcp.botbus.ai/mcp"

// welcomeState tracks whether the popup is currently rendered over the chat
// view, and which copy variant to display.
type welcomeState struct {
	visible bool
	fresh   bool // true → "new channel" header; false → "welcome to channel"
}

// welcomeMarkerPath returns the absolute path to the empty marker file used
// to gate "have I already welcomed this user on this channel?". Cross-
// platform via os.UserConfigDir (XDG_CONFIG_HOME on Linux, ~/Library/Application
// Support on macOS, %AppData% on Windows). Returns "" if UserConfigDir fails
// — callers treat that as "always show" (better to over-popup than to crash).
func welcomeMarkerPath(channelID string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "botbus", "welcomed", channelID)
}

// isWelcomed reports whether the marker file for the given channel exists.
// Any I/O error (missing dir, permission denied, …) is treated as "not yet
// welcomed" — the popup is informational, so erring on the side of showing
// it is safe.
func isWelcomed(channelID string) bool {
	p := welcomeMarkerPath(channelID)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// markWelcomed creates the empty marker file for the given channel. Returns
// any error from MkdirAll/WriteFile so callers can surface it for debugging,
// but the caller's expected behavior on error is to ignore — re-popping the
// welcome on next visit is acceptable.
func markWelcomed(channelID string) error {
	p := welcomeMarkerPath(channelID)
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, nil, 0o644)
}

// channelIDFromHost strips the .botbus.ai suffix to recover the channel ID.
// If the host doesn't end in .botbus.ai (custom domain / test host), the
// host is returned verbatim.
func channelIDFromHost(host string) string {
	return strings.TrimSuffix(host, "."+welcomeDomain)
}

// renderWelcomeContent assembles the plain (un-bordered) body of the popup
// for the given channel. Public so the test can verify the exact strings —
// formatting/borders happen in renderWelcomePopup.
func renderWelcomeContent(channelID string, fresh bool) string {
	header := "Welcome to this private channel."
	if fresh {
		header = "Your new private channel."
	}
	channelURL := "https://" + channelID + "." + welcomeDomain + "/"
	return strings.Join([]string{
		header,
		"botbus.ai · in-memory pub/sub · the URL is the secret",
		"",
		"channel: " + channelURL,
		"",
		"Share via web:",
		"  " + channelURL,
		"",
		"Connect Claude Code (or any MCP-capable agent):",
		"  claude mcp add --transport http botbus " + mcpGatewayURL,
		"",
		"Other agents:",
		"  ChatGPT:      Settings → Connectors → Add custom MCP → " + mcpGatewayURL,
		"  Codex CLI:    codex mcp add botbus --url " + mcpGatewayURL,
		"  Cursor:       ~/.cursor/mcp.json → {\"mcpServers\":{\"botbus\":{\"url\":\"" + mcpGatewayURL + "\"}}}",
		"  Gemini CLI:   gemini mcp add --transport http botbus " + mcpGatewayURL,
		"  Antigravity:  Settings → Customizations → MCP Config →",
		"                {\"mcpServers\":{\"botbus\":{\"serverUrl\":\"" + mcpGatewayURL + "\"}}}",
		"",
		"Direct interfaces:",
		"  subscribe: curl -N -H 'Accept: text/event-stream' " + channelURL,
		"  publish:   curl -X POST " + channelURL + " --data 'name: hello'",
		"  websocket: wss://" + channelID + "." + welcomeDomain + "/",
	}, "\n")
}

// Lipgloss styles for the popup. The border + accent colors match the bar
// style elsewhere in the TUI so the popup feels native rather than dropped-
// in. titleStyle uses the same yellow as the bar's title text.
var (
	welcomeBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("117")).
				Padding(1, 2).
				Foreground(lipgloss.Color("252"))
	welcomeTitleStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("220")).
				Bold(true)
	welcomeURLStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("117")).
			Underline(true)
	welcomeHintStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("243")).
				Italic(true)
)

// renderWelcomePopup wraps the content from renderWelcomeContent in a styled
// box with the title styled distinctly and a footer hint. Sized to fit the
// terminal width: if the terminal is narrower than the content, lipgloss
// soft-wraps inside the border.
func renderWelcomePopup(channelID string, fresh bool, termWidth int) string {
	content := renderWelcomeContent(channelID, fresh)
	lines := strings.Split(content, "\n")
	// First line is the header — style it distinctly.
	if len(lines) > 0 {
		lines[0] = welcomeTitleStyle.Render(lines[0])
	}
	// Style the channel: URL line so the user's eye lands on it.
	for i, ln := range lines {
		if strings.HasPrefix(ln, "channel: ") {
			lines[i] = "channel: " + welcomeURLStyle.Render(strings.TrimPrefix(ln, "channel: "))
		}
	}
	body := strings.Join(lines, "\n")
	footer := welcomeHintStyle.Render("press Esc / Enter / Ctrl-H to dismiss · Ctrl-H to re-show")

	// Constrain border width so it doesn't blow past the terminal. Leave 4
	// columns of headroom for the padding + border itself.
	w := termWidth - 4
	if w < 30 {
		w = 30 // graceful narrow-terminal fallback; lipgloss will wrap
	}
	return welcomeBorderStyle.Width(w).Render(body + "\n\n" + footer)
}
