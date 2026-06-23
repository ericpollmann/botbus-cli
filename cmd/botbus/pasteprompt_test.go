package main

import (
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
)

func TestLocalPastePromptContainsMCPAndChannel(t *testing.T) {
	inst := daemon.ConnectInstructions{
		MCPCommand: "claude mcp add --transport http mythwork http://127.0.0.1:8765/a/k1",
		ChannelURL: "https://chan.botbus.ai/",
	}
	got := localPastePrompt("mythwork", "workspace owner", inst)
	for _, want := range []string{inst.MCPCommand, inst.ChannelURL, "mythwork", "workspace owner"} {
		if !strings.Contains(got, want) {
			t.Fatalf("localPastePrompt missing %q in:\n%s", want, got)
		}
	}
}

func TestLocalPastePromptDefaultsRole(t *testing.T) {
	got := localPastePrompt("agentx", "", daemon.ConnectInstructions{MCPCommand: "x", ChannelURL: "y"})
	if !strings.Contains(got, "agentx") || strings.Contains(got, "()") {
		t.Fatalf("empty role should yield a sensible default, got:\n%s", got)
	}
}

func TestInvitePastePromptContainsJoinURLAndUser(t *testing.T) {
	got := invitePastePrompt("ethan", "https://abc.botbus.ai/?user=ethan")
	for _, want := range []string{"ethan", "https://abc.botbus.ai/?user=ethan"} {
		if !strings.Contains(got, want) {
			t.Fatalf("invitePastePrompt missing %q in:\n%s", want, got)
		}
	}
}
