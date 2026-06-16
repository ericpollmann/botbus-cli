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

func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	hubBase := fs.String("hub", envOr("HUB_BASE", "https://"+domain), "hub base URL")
	hubDomain := fs.String("hub-domain", envOr("HUB_DOMAIN", domain), "hub apex domain")
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
