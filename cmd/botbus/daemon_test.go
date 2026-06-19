package main

import (
	"strings"
	"testing"
)

// resolveRouterURL implements the daemon's router-URL precedence: an explicit
// --router flag wins over the ROUTER_URL env, which wins over the persisted
// state.daemon.router_url, which finally falls back to DefaultRouterURL. Each
// arg is the already-resolved value for that source ("" = unset). TDD: this
// test is written before the function exists so it fails to compile first.
func TestResolveRouterURL(t *testing.T) {
	const (
		flagURL  = "https://flag.example"
		envURL   = "https://env.example"
		stateURL = "https://state.example"
		defURL   = DefaultRouterURL
	)
	cases := []struct {
		name                  string
		flag, env, state, def string
		want                  string
	}{
		{"flag wins over all", flagURL, envURL, stateURL, defURL, flagURL},
		{"env beats state and def", "", envURL, stateURL, defURL, envURL},
		{"state beats def", "", "", stateURL, defURL, stateURL},
		{"def fallback when others empty", "", "", "", defURL, defURL},
		{"all empty yields empty", "", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveRouterURL(c.flag, c.env, c.state, c.def); got != c.want {
				t.Errorf("resolveRouterURL(%q,%q,%q,%q) = %q, want %q",
					c.flag, c.env, c.state, c.def, got, c.want)
			}
		})
	}
}

// DefaultRouterURL must point at the live, branded router over HTTPS — not a
// localhost fallback (the localhost default was the root cause of the daemon
// "unsupported protocol scheme" spam when no ROUTER_URL/state value existed)
// and not the raw .fly.dev origin (we ship the branded router.botbus.ai
// hostname so the CLI never exposes the underlying Fly app).
func TestDefaultRouterURLIsLive(t *testing.T) {
	if DefaultRouterURL != "https://router.botbus.ai" {
		t.Errorf("DefaultRouterURL = %q, want the branded live router", DefaultRouterURL)
	}
	if !strings.HasPrefix(DefaultRouterURL, "https://") {
		t.Errorf("DefaultRouterURL = %q, must use https", DefaultRouterURL)
	}
	if strings.Contains(DefaultRouterURL, "localhost") || strings.Contains(DefaultRouterURL, "127.0.0.1") {
		t.Errorf("DefaultRouterURL = %q, must not be a localhost fallback", DefaultRouterURL)
	}
	if strings.Contains(DefaultRouterURL, ".fly.dev") {
		t.Errorf("DefaultRouterURL = %q, must use the branded hostname, not the raw .fly.dev origin", DefaultRouterURL)
	}
}
