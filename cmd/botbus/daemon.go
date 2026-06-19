package main

// daemon.go — the `botbus daemon` subcommand. Runs the multiplexing host
// daemon: one inbox subscription + delivery queue + localhost MCP endpoint per
// agent in local state, plus re-register + heartbeat against the router.
// Dispatched only when argv[1] == "daemon".

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// resolveRouterURL picks the control-plane base for the daemon, returning the
// first non-empty of: an explicit --router flag, the ROUTER_URL env, the
// persisted state.daemon.router_url, and finally the compiled-in default. Pure
// (no env/flag reads of its own) so the precedence is unit-testable; callers
// pass each source's already-resolved value ("" = unset).
func resolveRouterURL(flag, env, state, def string) string {
	for _, v := range []string{flag, env, state, def} {
		if v != "" {
			return v
		}
	}
	return ""
}

func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	hubBase := fs.String("hub", envOr("HUB_BASE", "https://"+domain), "hub base URL")
	hubDomain := fs.String("hub-domain", envOr("HUB_DOMAIN", domain), "hub apex domain")
	// --router has an empty default so an unset flag stays "" and yields to
	// ROUTER_URL / state / DefaultRouterURL in resolveRouterURL's precedence.
	router := fs.String("router", "", "router control API base URL (overrides ROUTER_URL and state)")
	_ = fs.Parse(args)

	statePath := agentstate.DefaultPath()
	st, err := agentstate.Load(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon: load state:", err)
		os.Exit(1)
	}
	if len(st.Agents) == 0 {
		fmt.Fprintln(os.Stderr, "daemon: no agents in", statePath, "— create one with 'botbus agent create'")
		os.Exit(1)
	}

	// Resolve the router URL with flag > env > state > default precedence and
	// install it in memory so daemon.Run (which reads d.state.Daemon.RouterURL)
	// builds a control client against it. Runtime resolution only — never
	// persisted back to the state file.
	st.Daemon.RouterURL = resolveRouterURL(*router, os.Getenv("ROUTER_URL"), st.Daemon.RouterURL, DefaultRouterURL)

	hub := hubclient.NewHTTPClient(*hubBase, *hubDomain)
	d := daemon.New(st, statePath, hub)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("botbus daemon: %d agent(s), MCP on %s\n", len(st.Agents), d.Addr())
	for _, a := range st.Agents {
		fmt.Printf("  %s  ->  http://%s/a/%s\n", a.ID, d.Addr(), a.Key)
	}
	if err := d.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "daemon:", err)
		os.Exit(1)
	}
}
