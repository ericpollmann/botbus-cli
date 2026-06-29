package main

import "testing"

func TestResolveChannelMode(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		desc    string
		channel string
		env     map[string]string
		want    string
		wantOk  bool
	}{
		{"positional wins over env", "abc", map[string]string{"BOTBUS_CHANNEL": "zzz"}, "abc", true},
		{"env fallback when no positional", "", map[string]string{"BOTBUS_CHANNEL": "zzz"}, "zzz", true},
		{"env value is trimmed", "", map[string]string{"BOTBUS_CHANNEL": "  zzz  "}, "zzz", true},
		{"neither set → not ok (must not mint)", "", map[string]string{}, "", false},
		{"blank env → not ok", "", map[string]string{"BOTBUS_CHANNEL": "   "}, "", false},
	}
	for _, c := range cases {
		got, ok := resolveChannelMode(c.channel, env(c.env))
		if got != c.want || ok != c.wantOk {
			t.Errorf("%s: resolveChannelMode(%q) = (%q, %v); want (%q, %v)",
				c.desc, c.channel, got, ok, c.want, c.wantOk)
		}
	}
}
