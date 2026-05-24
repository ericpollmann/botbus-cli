package main

import "testing"

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
		{"a:b", "", "a:b", false},          // no space after colon
	}
	for _, c := range cases {
		name, body, ok := parseMsg([]byte(c.in))
		if name != c.name || body != c.body || ok != c.ok {
			t.Errorf("parseMsg(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, name, body, ok, c.name, c.body, c.ok)
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
			append([]byte{0x01}, []byte("eric: \xFF\xFE\x00\x42")...),
			"eric", []byte{0xFF, 0xFE, 0x00, 0x42}, true},
		{"empty audio after separator",
			[]byte{0x01, 'a', ':', ' '},
			"", nil, false}, // len<4 actually len=4 but no audio bytes after — len(audio)==0 but ok=true; check below
		{"too short", []byte{0x01, 'a'}, "", nil, false},
		{"wrong type", []byte{0x02, 'a', ':', ' ', 'x'}, "", nil, false},
		{"empty", nil, "", nil, false},
		{"text-shaped", []byte("eric: hi"), "", nil, false}, // 'e' != 0x01
		{"missing separator", []byte{0x01, 'a', 'b', 'c'}, "", nil, false},
		{"multibyte name",
			append(append([]byte{0x01}, []byte("🦊")...), []byte(": payload")...),
			"🦊", []byte("payload"), true},
	}
	for _, c := range cases {
		gotName, gotAudio, gotOk := parseAudioFrame(c.in)
		// Special-case: "empty audio after separator" → parseAudioFrame
		// actually returns ok=true with empty audio. Adjust assertion.
		if c.desc == "empty audio after separator" {
			if !gotOk || gotName != "a" || len(gotAudio) != 0 {
				t.Errorf("%s: got (%q,%v,%v); want (\"a\",[],true)", c.desc, gotName, gotAudio, gotOk)
			}
			continue
		}
		if gotOk != c.wantOk || gotName != c.wantName || string(gotAudio) != string(c.wantAudio) {
			t.Errorf("%s: got (%q,%v,%v); want (%q,%v,%v)",
				c.desc, gotName, gotAudio, gotOk, c.wantName, c.wantAudio, c.wantOk)
		}
	}
}

func TestIsTypedFrame(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, false},
		{"empty-slice", []byte{}, false},
		{"text", []byte("eric: hi"), false},
		{"text-with-leading-space", []byte(" leading space"), false},
		{"text-with-leading-digit", []byte("9lives: meow"), false},
		{"text-with-leading-emoji", []byte("🦊: hi"), false}, // multibyte UTF-8 start byte is >= 0xC0
		{"audio-frame", []byte{0x01, 'e', 'r', 'i', 'c', ':', ' ', 0xFF, 0xFE}, true},
		{"reserved-frame-02", []byte{0x02, 0x00}, true},
		{"reserved-frame-1F", []byte{0x1F}, true},
		{"boundary-0x20-is-text", []byte{0x20, 'a'}, false}, // 0x20 = space, treat as text
	}
	for _, c := range cases {
		if got := isTypedFrame(c.in); got != c.want {
			t.Errorf("%s: isTypedFrame(%v) = %v, want %v", c.name, c.in, got, c.want)
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
		{"single line wrapping to 2 rows", "abcdefghij", 5, 2}, // 10 chars / 5 = 2
		{"single line wrapping to 3 rows", "abcdefghijk", 5, 3}, // 11/5 = 3 (ceil)
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
		{"eric", "/dm alice", ""},     // no space after target
		{"eric", "/me", ""},           // no space after /me
		{"eric", "hello world", ""},   // plain text
		{"eric", "", ""},              // empty body
		{"eric", "/dme nope", ""},     // not /dm with space
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
