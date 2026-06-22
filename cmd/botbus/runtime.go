package main

// runtime.go — the shared local-agent runtime constructor used by both the
// console TUI and the MCP face. Task 9 extends this file with
// ensureSingleRuntime / runAll / RunOn helpers.

import (
	"context"
	"fmt"
	"net"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/keys"
)

// ensureSingleRuntime is the per-host mutex: it binds the runtime's MCP port so
// a second runtime (e.g. `botbus daemon` while `botbus` is open) fails fast
// instead of double-subscribing every inbox and colliding on the port.
func ensureSingleRuntime(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("a botbus runtime is already running on %s — stop it first", addr)
	}
	return ln, nil
}

// runAll serves the runtime's faces — inbox loops + per-agent MCP mux — on the
// pre-bound listener, so the single-runtime port-mutex is held continuously
// from preflight through serve. The TUI, when present, is run by the caller
// (runConsole) alongside this. Uses RunOn to serve on a bound listener.
func runAll(ctx context.Context, rt *daemon.Daemon, ln net.Listener) error {
	return rt.RunOn(ctx, ln)
}

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
