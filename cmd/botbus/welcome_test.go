package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChannelIDFromHost(t *testing.T) {
	cases := map[string]string{
		"abc.botbus.ai":       "abc",
		"cp3pby4z2.botbus.ai": "cp3pby4z2",
		"botbus.ai":           "botbus.ai", // no subdomain
		"localhost:8080":      "localhost:8080",
		"":                    "",
	}
	for in, want := range cases {
		if got := channelIDFromHost(in); got != want {
			t.Errorf("channelIDFromHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderWelcomeContentFresh(t *testing.T) {
	const id = "cp3pby4z24esp970de401m5tx4"
	body := renderWelcomeContent(id, true)
	mustContain(t, body, "Your new private channel.")
	mustContain(t, body, "https://"+id+".botbus.ai/")
	mustContain(t, body, "claude mcp add --transport http botbus https://mcp.botbus.ai/mcp")
	mustContain(t, body, "codex mcp add botbus --url https://mcp.botbus.ai/mcp")
	mustContain(t, body, "gemini mcp add --transport http botbus https://mcp.botbus.ai/mcp")
	mustContain(t, body, "Antigravity:")
	mustContain(t, body, "wss://"+id+".botbus.ai/")
	mustNotContain(t, body, "Welcome to this private channel.")
}

func TestRenderWelcomeContentReturning(t *testing.T) {
	const id = "cp3pby4z24esp970de401m5tx4"
	body := renderWelcomeContent(id, false)
	mustContain(t, body, "Welcome to this private channel.")
	mustNotContain(t, body, "Your new private channel.")
}

// Marker-file gating: redirect UserConfigDir to a temp dir via XDG_CONFIG_HOME
// on Linux. On macOS UserConfigDir uses ~/Library/Application Support, which
// honors HOME but not XDG. Make this test cross-platform by computing the
// expected path through welcomeMarkerPath itself, then asserting on that.
func TestWelcomeMarkerLifecycle(t *testing.T) {
	tmp := t.TempDir()
	// XDG_CONFIG_HOME → tmp on Linux; HOME → tmp on macOS so Library/App
	// Support lands underneath. Set both to be safe across CI environments.
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	const id = "test-channel-id"
	if isWelcomed(id) {
		t.Fatal("expected fresh channel to be un-welcomed")
	}
	if err := markWelcomed(id); err != nil {
		t.Fatalf("markWelcomed: %v", err)
	}
	if !isWelcomed(id) {
		t.Fatal("expected channel to be welcomed after markWelcomed")
	}
	// Verify the file landed somewhere under tmp (sanity check that UserConfigDir
	// actually picked up our env override).
	p := welcomeMarkerPath(id)
	if !strings.HasPrefix(p, tmp) {
		t.Errorf("marker path %q not under tmp dir %q", p, tmp)
	}
	// And the file itself exists.
	if _, err := os.Stat(p); err != nil {
		t.Errorf("marker file missing: %v", err)
	}
	// Idempotency: marking twice doesn't error.
	if err := markWelcomed(id); err != nil {
		t.Fatalf("markWelcomed second call: %v", err)
	}
}

// renderWelcomePopup must contain the content body plus a "press … dismiss"
// footer. The border characters are lipgloss-rendered ANSI — assertions on
// raw substrings work because the content text isn't ANSI-escaped (only
// styled lines are).
func TestRenderWelcomePopupShape(t *testing.T) {
	const id = "cp3pby4z24esp970de401m5tx4"
	out := renderWelcomePopup(id, true, 80)
	mustContain(t, out, "https://"+id+".botbus.ai/")
	mustContain(t, out, "dismiss")
	mustContain(t, out, "Ctrl-H")
	// Narrow terminals shouldn't crash — verify the 30-col fallback path runs.
	// Content gets soft-wrapped across border lines, so the full URL may not
	// appear contiguously; just assert the channel ID substring is present.
	narrow := renderWelcomePopup(id, false, 10)
	mustContain(t, narrow, "cp3pby4z24esp970")
	mustContain(t, narrow, "dismiss")
}

// mustContain / mustNotContain — simple test helpers; the existing test file
// uses inline asserts so we define our own here. Filenames differ so there's
// no collision.
func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("body missing %q\n--- body ---\n%s\n--- end ---", want, body)
	}
}
func mustNotContain(t *testing.T, body, want string) {
	t.Helper()
	if strings.Contains(body, want) {
		t.Errorf("body unexpectedly contains %q", want)
	}
}

// Defensive: filepath import used only inside tests on the marker path.
var _ = filepath.Join

// TestModelWelcomeFreshAutoShows: a model built with fresh=true should have
// welcome.visible set, regardless of the per-channel marker file.
func TestModelWelcomeFreshAutoShows(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
	// Even when the channel was previously welcomed, fresh=true overrides.
	const id = "fresh-test-channel-id"
	if err := markWelcomed(id); err != nil {
		t.Fatal(err)
	}
	recv := make(chan []byte)
	states := make(chan connState)
	send := make(chan []byte)
	m := newModel(id+".botbus.ai", "me", true, recv, states, send)
	if !m.welcome.visible {
		t.Error("fresh=true should auto-show the welcome popup even when marker exists")
	}
	if !m.welcome.fresh {
		t.Error("welcome.fresh should be true so the 'new channel' header copy fires")
	}
}

// TestModelWelcomeReturningGated: a model built with fresh=false should
// auto-show ONLY when the per-channel marker is absent.
func TestModelWelcomeReturningGated(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
	const id = "returning-test-channel-id"
	recv := make(chan []byte)
	states := make(chan connState)
	send := make(chan []byte)

	// First visit: marker absent → should show.
	m1 := newModel(id+".botbus.ai", "me", false, recv, states, send)
	if !m1.welcome.visible {
		t.Error("first visit (no marker) should auto-show")
	}
	if m1.welcome.fresh {
		t.Error("fresh=false should yield welcome.fresh=false for 'welcome to channel' copy")
	}

	// Mark welcomed, then build a fresh model — should NOT show.
	if err := markWelcomed(id); err != nil {
		t.Fatal(err)
	}
	m2 := newModel(id+".botbus.ai", "me", false, recv, states, send)
	if m2.welcome.visible {
		t.Error("second visit (marker present) should not auto-show")
	}
}
