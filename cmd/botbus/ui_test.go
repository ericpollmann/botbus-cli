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
