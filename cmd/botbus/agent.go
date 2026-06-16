package main

// agent.go — the `botbus agent <create|list|remove>` subcommands. These manage
// routing-fabric agent identities (key + inbox channel + local state) and
// register them with the router's control API. Distinct from the chat TUI in
// main.go: dispatched only when argv[1] == "agent".

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/keys"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func realDeps() hostagent.Deps {
	return hostagent.Deps{
		Hub:       hubclient.NewHTTPClient(envOr("HUB_BASE", "https://"+domain), envOr("HUB_DOMAIN", domain)),
		Control:   control.NewClient(envOr("ROUTER_URL", "http://127.0.0.1:8090")),
		StatePath: agentstate.DefaultPath(),
		MintKey:   keys.New,
	}
}

// agentCmd handles `botbus agent <sub> [flags]`.
func agentCmd(args []string) {
	if len(args) < 1 {
		agentUsage()
		os.Exit(2)
	}
	ctx := context.Background()
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("agent create", flag.ExitOnError)
		id := fs.String("name", "", "agent id / addressable handle (required)")
		focus := fs.String("focus", "", "platform focus-area description")
		mode := fs.String("mode", "session", "delivery mode: session|spawn")
		parent := fs.String("parent", "", "escalation target agent id")
		tier := fs.String("tier", "", "model tier label (opus|sonnet|haiku|fable)")
		_ = fs.Parse(args[1:])
		a, err := hostagent.Create(ctx, realDeps(), hostagent.CreateOpts{
			ID: *id, Focus: *focus, Mode: *mode, Parent: *parent, ModelTier: *tier,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		fmt.Printf("created agent %q\n  inbox channel: %s\n  mode: %s\n", a.ID, a.InboxChannel, a.Mode)
		fmt.Println("  key stored in", agentstate.DefaultPath())
	case "list":
		agents, err := hostagent.List(agentstate.DefaultPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "list:", err)
			os.Exit(1)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tMODE\tINBOX\tFOCUS")
		for _, a := range agents {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.ID, a.Mode, a.InboxChannel, a.Focus)
		}
		tw.Flush()
	case "remove":
		fs := flag.NewFlagSet("agent remove", flag.ExitOnError)
		id := fs.String("name", "", "agent id to remove (required)")
		_ = fs.Parse(args[1:])
		if err := hostagent.Remove(agentstate.DefaultPath(), *id); err != nil {
			fmt.Fprintln(os.Stderr, "remove:", err)
			os.Exit(1)
		}
		fmt.Printf("removed agent %q from local state\n", *id)
	default:
		agentUsage()
		os.Exit(2)
	}
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, "usage: botbus agent <create|list|remove> [flags]")
}
