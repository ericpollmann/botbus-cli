package main

// pasteprompt.go — builds the ready-to-paste prompts the onboarding wizard
// prints. localPastePrompt is for identities reachable via the operator's
// LOCAL botbus MCP (the operator's own session, standing agents on this
// machine); invitePastePrompt is the message to send a teammate on another
// machine (their join URL is their credential).

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
)

// sanitizeMCPKey returns a TOML-safe bare key derived from name: only
// [A-Za-z0-9_-] are kept; any run of disallowed characters is collapsed to a
// single "-"; leading/trailing "-" are trimmed. Falls back to "agent" if the
// result is empty.
var unsafeRun = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func sanitizeMCPKey(name string) string {
	key := unsafeRun.ReplaceAllString(name, "-")
	key = strings.Trim(key, "-")
	if key == "" {
		return "agent"
	}
	return key
}

func localPastePrompt(name, role string, inst daemon.ConnectInstructions) string {
	if role == "" {
		role = "an agent"
	}
	mcpKey := sanitizeMCPKey(name)
	codexBlock := fmt.Sprintf("[mcp_servers.%s]\nurl = %q", mcpKey, inst.MCPEndpoint)
	return fmt.Sprintf(`Connect "%s" (%s) to botbus — use the block for your coding agent:

── Claude Code ──
%s

── Codex (~/.codex/config.toml) ──
%s

1. Read your briefing: %s
2. Follow it. Post status (task.started / task.blocked / task.done) to your
   channel and watch the team board at the workspace root.`,
		name, role,
		inst.MCPCommand,
		codexBlock,
		inst.ChannelURL)
}

func invitePastePrompt(user, joinURL string) string {
	return fmt.Sprintf(`Send this to %s — the URL is their credential to join:

  %s

They install botbus (go install github.com/ericpollmann/botbus-cli/cmd/botbus@latest)
and run:  botbus attach %s`,
		user, joinURL, joinURL)
}
