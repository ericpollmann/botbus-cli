package main

import (
	"strings"
	"testing"
)

func TestResolveURL_NonMinting(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Full URL — use as-is (slash normalized).
		{"https://abc.botbus.ai/", "https://abc.botbus.ai/"},
		{"https://abc.botbus.ai", "https://abc.botbus.ai/"},
		{"http://abc.botbus.ai/", "http://abc.botbus.ai/"},
		// Hostname (contains a dot) — prepend https://.
		{"abc.botbus.ai", "https://abc.botbus.ai/"},
		{"abc.botbus.ai/", "https://abc.botbus.ai/"},
		// Bare channel ID — append .botbus.ai.
		{"abc", "https://abc.botbus.ai/"},
		{"abc/", "https://abc.botbus.ai/"},
	}
	for _, c := range cases {
		got, err := resolveURL(c.in)
		if err != nil {
			t.Errorf("resolveURL(%q) err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseMsg(t *testing.T) {
	cases := []struct {
		in   string
		name string
		body string
		ok   bool
	}{
		{"eric: hello", "eric", "hello", true},
		{"eric: hello: world", "eric", "hello: world", true},
		{"plain text", "", "plain text", false},
		{"", "", "", false},
		{": leading colon", "", ": leading colon", false}, // i==0 → no split, raw text
		{"a:b", "", "a:b", false},                         // no space after colon
		{"eric: hello [id 1]", "eric", "hello", true},     // ID suffix stripped from body
		{"eric: hello [id zs]", "eric", "hello", true},
	}
	for _, c := range cases {
		name, body, _, ok := parseMsgWithID([]byte(c.in))
		if name != c.name || body != c.body || ok != c.ok {
			t.Errorf("parseMsgWithID(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, name, body, ok, c.name, c.body, c.ok)
		}
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		desc        string
		argv        []string
		wantCh      string
		wantMonitor bool
		wantName    string
	}{
		{"empty", nil, "", false, ""},
		{"just channel", []string{"abc123"}, "abc123", false, ""},
		{"monitor flag alone", []string{"--monitor"}, "", true, ""},
		{"monitor + channel + name",
			[]string{"--monitor", "abc123", "--name", "alpha"},
			"abc123", true, "alpha"},
		{"channel before monitor",
			[]string{"abc123", "--monitor", "--name", "beta"},
			"abc123", true, "beta"},
		{"name alone (no monitor)",
			[]string{"abc", "--name", "gamma"},
			"abc", false, "gamma"},
		{"--name without value is ignored",
			[]string{"abc", "--name"}, "abc", false, ""},
		{"extra positional ignored",
			[]string{"abc", "extra"}, "abc", false, ""},
		// --listen is the README spelling (alias for --monitor). This exact
		// invocation used to be mis-parsed (--listen became the channel), so
		// headless mode never triggered and the TUI died with a TTY error.
		{"listen alias alone", []string{"--listen"}, "", true, ""},
		{"README form: --listen <id> --skip NAME",
			[]string{"--listen", "abc123", "--skip", "alpha"},
			"abc123", true, "alpha"},
		{"--skip is an alias for --name",
			[]string{"abc", "--skip", "delta"}, "abc", false, "delta"},
		{"channel before --listen",
			[]string{"abc123", "--listen", "--name", "eps"},
			"abc123", true, "eps"},
	}
	for _, c := range cases {
		got := parseArgs(c.argv)
		if got.channel != c.wantCh || got.monitor != c.wantMonitor || got.name != c.wantName {
			t.Errorf("%s: got %+v; want channel=%q monitor=%v name=%q",
				c.desc, got, c.wantCh, c.wantMonitor, c.wantName)
		}
	}
}

func TestMonitorBanner(t *testing.T) {
	got := monitorBanner("abc123", "alpha")
	// Sanity: includes channel id, name, and key MCP tool names.
	for _, want := range []string{
		"abc123", "alpha",
		"mcp__botbus__set_name", "mcp__botbus__subscribe", "mcp__botbus__send",
		"https://mcp.botbus.ai",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("monitorBanner missing %q in:\n%s", want, got)
		}
	}
}

func TestParseAudioFrame(t *testing.T) {
	cases := []struct {
		desc      string
		in        []byte
		wantName  string
		wantAudio []byte
		wantOk    bool
	}{
		{"valid eric+payload",
			[]byte("eric: \xFF\xFE\x00\x42"),
			"eric", []byte{0xFF, 0xFE, 0x00, 0x42}, true},
		{"empty audio after separator",
			[]byte("a: "),
			"a", []byte{}, true},
		{"too short", []byte("a"), "", nil, false},
		{"empty", nil, "", nil, false},
		{"missing separator", []byte("abcdef"), "", nil, false},
		{"empty name", []byte(": payload"), "", nil, false},
		{"multibyte name",
			append([]byte("🦊"), []byte(": payload")...),
			"🦊", []byte("payload"), true},
	}
	for _, c := range cases {
		gotName, gotAudio, gotOk := parseAudioFrame(c.in)
		if gotOk != c.wantOk || gotName != c.wantName || string(gotAudio) != string(c.wantAudio) {
			t.Errorf("%s: got (%q,%v,%v); want (%q,%v,%v)",
				c.desc, gotName, gotAudio, gotOk, c.wantName, c.wantAudio, c.wantOk)
		}
	}
}

func TestVisualRows(t *testing.T) {
	cases := []struct {
		desc  string
		value string
		width int
		want  int
	}{
		{"empty", "", 80, 1},
		{"width-zero short-circuit", "anything", 0, 1},
		{"width-negative", "anything", -5, 1},
		{"one short line", "hello", 80, 1},
		{"two short lines via explicit newline", "hello\nworld", 80, 2},
		{"three lines incl. blank middle", "a\n\nb", 80, 3},
		{"single line wrapping to 2 rows", "abcdefghij", 5, 2},     // 10 chars / 5 = 2
		{"single line wrapping to 3 rows", "abcdefghijk", 5, 3},    // 11/5 = 3 (ceil)
		{"mixed: one short, one wrapping", "hi\nabcdefghij", 5, 3}, // 1 + 2
		{"exact width", "12345", 5, 1},
		{"one over", "123456", 5, 2},
	}
	for _, c := range cases {
		if got := visualRows(c.value, c.width); got != c.want {
			t.Errorf("%s: visualRows(%q, %d) = %d, want %d", c.desc, c.value, c.width, got, c.want)
		}
	}
}

func TestRenderSlash(t *testing.T) {
	// Exact rendered output depends on lipgloss color codes which vary by
	// terminal profile in tests. Strip ANSI and look for the textual content.
	strip := func(s string) string {
		// Remove escape sequences with a simple state machine.
		var out []byte
		inEsc := false
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !inEsc && c == 0x1b {
				inEsc = true
				continue
			}
			if inEsc {
				if c == 'm' {
					inEsc = false
				}
				continue
			}
			out = append(out, c)
		}
		return string(out)
	}
	cases := []struct {
		name string
		body string
		want string // empty = expected ok=false
	}{
		{"eric", "/me nods", "* eric nods"},
		{"eric", "/me nods head vigorously", "* eric nods head vigorously"},
		{"eric", "/dm alice hello there", "eric → alice: hello there"},
		{"eric", "/dm alice", ""},        // no space after target
		{"eric", "/me", ""},              // no space after /me
		{"eric", "hello world", ""},      // plain text
		{"eric", "", ""},                 // empty body
		{"eric", "/dme nope", ""},        // not /dm with space
		{"eric", "/dm  doubleblank", ""}, // empty target rejected (sp at index 0, sp > 0 false)
	}
	for _, c := range cases {
		got, ok := renderSlash(c.name, c.body, nameColor(c.name))
		if c.want == "" {
			if ok {
				t.Errorf("renderSlash(%q,%q): expected fallthrough, got %q", c.name, c.body, strip(got))
			}
			continue
		}
		if !ok {
			t.Errorf("renderSlash(%q,%q): expected slash render, got ok=false", c.name, c.body)
			continue
		}
		if g := strip(got); g != c.want {
			t.Errorf("renderSlash(%q,%q) = %q, want %q", c.name, c.body, g, c.want)
		}
	}
}

func TestNameColor(t *testing.T) {
	// Deterministic — same name always same color, in range [0,16).
	for _, n := range []string{"", "eric", "joe", "anon-123", "a long name with spaces"} {
		c := nameColor(n)
		if c < 0 || c >= 16 {
			t.Errorf("nameColor(%q) = %d, out of range", n, c)
		}
		if c != nameColor(n) {
			t.Errorf("nameColor(%q) not deterministic", n)
		}
	}
	// "abc" = 97+98+99 = 294; 294 & 0x0F = 6.
	if got := nameColor("abc"); got != 6 {
		t.Errorf("nameColor(\"abc\") = %d, want 6", got)
	}
}

// userAgent must always start with "botbus" so the server's classifyUA
// buckets us as classCLI. The version suffix is best-effort: in test/devel
// builds debug.ReadBuildInfo returns an empty version, so we should see
// the explicit "devel" fallback.
func TestUserAgent(t *testing.T) {
	ua := userAgent()
	if !strings.HasPrefix(ua, "botbus-cli/") {
		t.Errorf("userAgent() = %q, want botbus-cli/ prefix", ua)
	}
	if !strings.Contains(strings.ToLower(ua), "botbus") {
		t.Errorf("userAgent() = %q, must contain 'botbus' for server-side classifyUA to bucket us as CLI", ua)
	}
}
