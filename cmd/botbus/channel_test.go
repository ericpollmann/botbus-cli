package main

import (
	"reflect"
	"testing"
)

func TestParseChannelSeeds(t *testing.T) {
	cases := []struct {
		desc       string
		positional string
		env        string
		want       []string
	}{
		{"both empty", "", "", nil},
		{"single positional", "abc", "", []string{"abc"}},
		{"env only", "", "xyz", []string{"xyz"}},
		{"comma list in env", "", "a,b,c", []string{"a", "b", "c"}},
		{"space + comma mix", "", "a, b\tc", []string{"a", "b", "c"}},
		{"positional then env, order preserved", "a", "b,c", []string{"a", "b", "c"}},
		{"dedup across sources", "a", "a,b,a", []string{"a", "b"}},
		{"full urls and hosts pass through", "", "https://a.botbus.ai/, b.botbus.ai", []string{"https://a.botbus.ai/", "b.botbus.ai"}},
	}
	for _, c := range cases {
		got := parseChannelSeeds(c.positional, c.env)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: parseChannelSeeds(%q, %q) = %v; want %v", c.desc, c.positional, c.env, got, c.want)
		}
	}
}
