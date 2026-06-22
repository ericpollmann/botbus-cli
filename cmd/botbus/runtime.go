package main

// runtime.go — the shared local-agent runtime constructor used by both the
// console TUI and the MCP face. Task 9 extends this file with
// ensureSingleRuntime / runAll / RunOn helpers.

import (
	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/keys"
)

// buildRuntime constructs the one local-agent runtime shared by the TUI and
// MCP faces. (Task 9 adds ensureSingleRuntime/runAll/RunOn to this file.)
func buildRuntime(p *profile.Profile) *daemon.Daemon {
	st, _ := agentstate.Load(agentstate.DefaultPath())
	return daemon.NewRuntime(daemon.Config{
		State:     st,
		StatePath: agentstate.DefaultPath(),
		Hub:       hubclient.NewHTTPClient(envOr("HUB_BASE", "https://"+domain), envOr("HUB_DOMAIN", domain)),
		Control:   control.NewClient(envOr("ROUTER_URL", DefaultRouterURL)),
		Profile:   p,
		MintKey:   keys.New,
		Domain:    domain,
	})
}
