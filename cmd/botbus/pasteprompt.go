package main

// pasteprompt.go — builds the ready-to-paste Claude Code prompts the onboarding
// wizard prints. localPastePrompt is for identities reachable via the operator's
// LOCAL botbus MCP (the operator's own session, standing agents on this machine);
// invitePastePrompt is the message to send a teammate on another machine (their
// join URL is their credential).

import (
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
)

func localPastePrompt(name, role string, inst daemon.ConnectInstructions) string {
	if role == "" {
		role = "an agent"
	}
	return fmt.Sprintf(`Paste into a Claude Code session to make it "%s" (%s) on botbus:

1. Connect: %s
2. Read your briefing: %s
3. Follow it. Post status (task.started / task.blocked / task.done) to your
   channel and watch the team board at the workspace root.`,
		name, role, inst.MCPCommand, inst.ChannelURL)
}

func invitePastePrompt(user, joinURL string) string {
	return fmt.Sprintf(`Send this to %s — the URL is their credential to join:

  %s

They install botbus (go install github.com/ericpollmann/botbus-cli/cmd/botbus@latest)
and run:  botbus attach %s`,
		user, joinURL, joinURL)
}
