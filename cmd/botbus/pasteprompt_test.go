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

// --- New tests for Codex support ---

func TestLocalPastePromptContainsCodexToml(t *testing.T) {
	inst := daemon.ConnectInstructions{
		MCPCommand:  "claude mcp add --transport http mythwork http://127.0.0.1:8765/a/testkey",
		MCPEndpoint: "http://127.0.0.1:8765/a/testkey",
		ChannelURL:  "https://chan.botbus.ai/",
	}
	got := localPastePrompt("mythwork", "workspace owner", inst)

	// Must still contain the Claude command.
	if !strings.Contains(got, inst.MCPCommand) {
		t.Fatalf("missing Claude MCPCommand in:\n%s", got)
	}
	// Must mention the config file.
	if !strings.Contains(got, "~/.codex/config.toml") {
		t.Fatalf("missing ~/.codex/config.toml in:\n%s", got)
	}
	// Must contain a [mcp_servers. table header.
	if !strings.Contains(got, "[mcp_servers.") {
		t.Fatalf("missing [mcp_servers. header in:\n%s", got)
	}
	// Must contain the exact endpoint URL in a url = "..." line.
	wantURL := `url = "` + inst.MCPEndpoint + `"`
	if !strings.Contains(got, wantURL) {
		t.Fatalf("missing %q in:\n%s", wantURL, got)
	}
	// Must still contain ChannelURL and briefing verbs.
	if !strings.Contains(got, inst.ChannelURL) {
		t.Fatalf("missing ChannelURL in:\n%s", got)
	}
	for _, verb := range []string{"status", "board"} {
		if !strings.Contains(got, verb) {
			t.Fatalf("missing briefing verb %q in:\n%s", verb, got)
		}
	}
	// Intro must NOT say "Claude Code session" (tool-neutral now).
	if strings.Contains(got, "Paste into a Claude Code session") {
		t.Fatalf("intro is still Claude-specific, should be tool-neutral:\n%s", got)
	}
}

func TestLocalPastePromptCodexKeyFromName(t *testing.T) {
	// "botbus boss" → key should be "botbus-boss", no space in TOML header.
	inst := daemon.ConnectInstructions{
		MCPCommand:  "claude mcp add --transport http botbus-boss http://127.0.0.1:8765/a/k",
		MCPEndpoint: "http://127.0.0.1:8765/a/k",
		ChannelURL:  "https://chan.botbus.ai/",
	}
	got := localPastePrompt("botbus boss", "coordinator", inst)

	wantHeader := "[mcp_servers.botbus-boss]"
	if !strings.Contains(got, wantHeader) {
		t.Fatalf("expected TOML header %q in:\n%s", wantHeader, got)
	}
	badHeader := "[mcp_servers.botbus boss]"
	if strings.Contains(got, badHeader) {
		t.Fatalf("unsafe TOML header %q must not appear in:\n%s", badHeader, got)
	}
}

// --- Sanitization helper unit tests ---

func TestSanitizeMCPKey(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"mythwork", "mythwork"},
		{"my-agent", "my-agent"},
		{"botbus boss", "botbus-boss"},
		{"  leading trailing  ", "leading-trailing"},
		{"hello world agent", "hello-world-agent"},
		{"abc!@#def", "abc-def"},
		{"", "agent"},
		{"!@#$%", "agent"},
		{"--already-dashed--", "already-dashed"},
		{"a b  c", "a-b-c"},
	}
	for _, tc := range cases {
		got := sanitizeMCPKey(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeMCPKey(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
