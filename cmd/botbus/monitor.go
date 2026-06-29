package main

// monitor.go — headless agent-driven mode (`botbus --monitor <id>`).
//
// Designed to be wrapped by a Claude-Code-style "Monitor" task: peer messages
// arrive on stdout one per line as "name: body" and trigger task-
// notifications. The agent replies via the MCP gateway (separate process,
// see mcp.botbus.ai). stderr carries banners and connection-state breadcrumbs
// so stdout stays a clean stream of channel content.
//
// Audio frames are silently drained — agents don't currently handle voice.

import (
	"context"
	"fmt"
	"os"
)

// monitorBanner returns the stderr greeting printed once at monitor startup.
// It tells the wrapping agent which channel + name it's connected to and
// shows the exact MCP tool calls it should use to reply on this specific
// channel. Goes to stderr so stdout stays a clean stream of peer messages.
func monitorBanner(channelID, name string) string {
	return "" +
		"botbus monitor: connected to https://" + channelID + ".botbus.ai/ as \"" + name + "\"\n" +
		"\n" +
		"On Claude Code v2.1.80+, `botbus --channel " + channelID + "` pushes each\n" +
		"message straight into the session (no Monitor, no polling). Otherwise:\n" +
		"\n" +
		"Each peer message arrives on stdout as \"name: body\" and triggers a\n" +
		"task-notification. Reply via the botbus MCP gateway:\n" +
		"\n" +
		"  mcp__botbus__set_name(name=\"" + name + "\")\n" +
		"  mcp__botbus__subscribe(channel=\"" + channelID + "\")\n" +
		"  mcp__botbus__send(channel=\"" + channelID + "\", text=\"…\")\n" +
		"\n" +
		"If the botbus MCP gateway isn't yet configured in your environment:\n" +
		"  claude mcp add --transport http botbus https://mcp.botbus.ai\n" +
		"  (Codex: add [mcp_servers.botbus] url=\"https://mcp.botbus.ai\" to ~/.codex/config.toml)\n" +
		"\n"
}

// runMonitor pumps incoming text frames to stdout one per line as "name: body".
// Designed for agent wake-up loops: a Monitor wraps this command and gets
// notified on each peer message. The agent's own broadcasts (from `name`)
// are filtered so the agent doesn't notify on itself. If filter is non-empty,
// only messages from that sender are printed. Audio frames are dropped
// silently. State changes log to stderr so the wrapping Monitor doesn't see
// them as channel content.
func runMonitor(ctx context.Context, recv, audio <-chan []byte, states <-chan connState, name, filter string) {
	// Drain side channels so runWS never blocks on full buffers.
	go func() {
		for range audio {
		}
	}()
	go func() {
		for s := range states {
			switch s {
			case stConnecting:
				fmt.Fprintln(os.Stderr, "botbus: connecting…")
			case stConnected:
				fmt.Fprintln(os.Stderr, "botbus: connected")
			case stDown:
				fmt.Fprintln(os.Stderr, "botbus: disconnected, will retry")
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-recv:
			if !ok {
				return
			}
			from, body, _, named := parseMsgWithID(m)
			if !named {
				continue // raw, non-protocol — agents only act on "name: body"
			}
			if from == name {
				continue // our own broadcast (cross-connection); skip
			}
			if filter != "" && from != filter {
				continue // --filter: only print messages from the specified sender
			}
			fmt.Printf("%s: %s\n", from, body)
		}
	}
}
