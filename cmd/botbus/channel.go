package main

// channel.go — Claude Code Channel mode (`botbus --channel <id>`).
//
// Unlike --monitor (which prints "name: body" to stdout for a Claude Code
// Monitor task to scrape), this mode runs as an MCP server over stdio that
// Claude Code spawns directly. It declares the experimental "claude/channel"
// capability, so each incoming peer message is pushed to the live session as a
// notifications/claude/channel JSON-RPC notification — injected into the
// conversation as <channel source="botbus" name="…" channel="…">body</channel>
// without blocking a turn (the old next()/monitor approaches either froze the
// turn on a long-poll or relied on stdout line-scraping). A two-way "send" tool
// lets Claude reply back onto the channel.
//
// Requires Claude Code v2.1.80+ and registration as a channel (see README).

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// resolveChannelMode resolves the channel id for --channel mode. An explicit
// positional id wins; otherwise it falls back to $BOTBUS_CHANNEL so a static
// plugin .mcp.json (`botbus --channel`, no id baked in) works when Claude Code
// spawns it with that env set. Returns ok=false when neither is provided — the
// caller must error rather than fall through to resolveURL(""), which would
// MINT a brand-new channel (wrong for a channel listener).
func resolveChannelMode(channel string, getenv func(string) string) (string, bool) {
	if channel != "" {
		return channel, true
	}
	if v := strings.TrimSpace(getenv("BOTBUS_CHANNEL")); v != "" {
		return v, true
	}
	return "", false
}

// channelInstructions is added to Claude's system prompt at initialize. It
// tells Claude what the injected <channel> events look like and how to reply,
// per the channel contract's recommendation to ship handling guidance.
const channelInstructions = "Live chat from a botbus channel arrives as " +
	"<channel source=\"botbus\" name=\"SENDER\" channel=\"ID\">body</channel>. " +
	"To reply, call the botbus `send` tool with the text to post back. Send only " +
	"the reply body; do not echo the name/channel attributes."

// runChannel runs the stdio MCP channel server. The caller starts the shared WS
// pump (runWSText/runWSAudio) and hands us recv/audio/states/send; we push each
// peer text frame to the session as a claude/channel notification and expose a
// "send" tool for replies.
//
// audio and states are drained so the WS goroutines never block on full buffers
// (mirrors runMonitor). They must not be written anywhere: over stdio, stdout
// carries MCP protocol framing, so connection breadcrumbs are discarded rather
// than logged. Own broadcasts (from == name) and, when filter is set, any
// sender != filter are skipped before notifying.
func runChannel(ctx context.Context, recv, audio <-chan []byte, states <-chan connState, send chan<- []byte, name, filter, channelID string) error {
	go func() {
		for range audio {
		}
	}()
	go func() {
		for range states {
		}
	}()

	s := server.NewMCPServer("botbus", currentVersion(),
		server.WithExperimental(map[string]any{"claude/channel": map[string]any{}}),
		server.WithInstructions(channelInstructions),
		server.WithToolCapabilities(false),
	)

	// Two-way reply tool. Frame "name: text" exactly as the TUI's publish path
	// does (ui.go's model.publish) and hand it to the shared WS send channel.
	s.AddTool(mcp.NewTool("send",
		mcp.WithDescription("Send a message back to this botbus channel."),
		mcp.WithString("text", mcp.Required(), mcp.Description("Message body to post to the channel.")),
	), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		text := req.GetString("text", "")
		if text == "" {
			return mcp.NewToolResultError("text is required"), nil
		}
		select {
		case send <- []byte(name + ": " + text):
			return mcp.NewToolResultText("sent"), nil
		case <-ctx.Done():
			return mcp.NewToolResultError("channel closed"), nil
		}
	})

	// Fan incoming peer frames into channel notifications until recv closes
	// (ctx canceled → runWSText closes recv).
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-recv:
				if !ok {
					return
				}
				from, body, _, named := parseMsgWithID(m)
				if !named || from == name {
					continue // raw non-protocol frame, or our own echo
				}
				if filter != "" && from != filter {
					continue // --from gate: only inject this sender's messages
				}
				s.SendNotificationToAllClients("notifications/claude/channel",
					map[string]any{
						"content": body,
						"meta": map[string]any{
							"name":    from,
							"channel": channelID,
						},
					})
			}
		}
	}()

	// Serve MCP over stdio; blocks until stdin closes or the process is
	// signaled. ServeStdio installs its own SIGTERM/SIGINT handling.
	return server.ServeStdio(s)
}
