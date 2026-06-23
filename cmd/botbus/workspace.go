package main

// workspace.go — the `botbus workspace <create|invite|list>` subcommands. A
// workspace is anchored by an org-root agent (Parent==""); invited humans become
// members parented to that root. The real logic lives in small functions that
// take a hostagent.Deps so tests inject fakes + a temp state path; workspaceCmd
// just wires realDeps() and prints. Dispatched only when argv[1] == "workspace".

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
)

// workspaceCreate mints the org-root anchor for a workspace: an agent with no
// parent. Members are later invited under it.
func workspaceCreate(ctx context.Context, d hostagent.Deps, name string) (agentstate.Agent, error) {
	return hostagent.Create(ctx, d, hostagent.CreateOpts{Name: name})
}

// workspaceInvite finds the workspace's org-root by name and creates a member
// agent parented to it, returning the member's join URL. The join URL embeds the
// member's inbox channel as the host and carries the (url-escaped) user as a
// query param — it IS the member's credential.
func workspaceInvite(ctx context.Context, d hostagent.Deps, user, wsName string) (joinURL string, err error) {
	root, ok, err := hostagent.GetByName(d.StatePath, wsName)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no workspace named %q — create it first", wsName)
	}
	member, err := hostagent.Create(ctx, d, hostagent.CreateOpts{Name: user, Parent: root.ID})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s.%s/?user=%s", member.InboxChannel, domain, url.QueryEscape(user)), nil
}

// workspaceCmd handles `botbus workspace <sub> [args/flags]`.
func workspaceCmd(args []string) {
	if len(args) < 1 {
		workspaceUsage()
		os.Exit(2)
	}
	ctx := context.Background()
	switch args[0] {
	case "create":
		if len(args) < 2 || args[1] == "" {
			workspaceUsage()
			os.Exit(2)
		}
		name := args[1]
		a, err := workspaceCreate(ctx, realDeps(), name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		fmt.Printf("created workspace %q\n  root id: %s\n  channel: https://%s.%s\n", a.Name, a.ID, a.InboxChannel, domain)
	case "invite":
		fs := flag.NewFlagSet("workspace invite", flag.ExitOnError)
		ws := fs.String("workspace", "", "workspace name to invite into (required)")
		_ = fs.Parse(args[1:])
		if fs.NArg() < 1 || fs.Arg(0) == "" || *ws == "" {
			workspaceUsage()
			os.Exit(2)
		}
		user := fs.Arg(0)
		joinURL, err := workspaceInvite(ctx, realDeps(), user, *ws)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invite:", err)
			os.Exit(1)
		}
		fmt.Println(joinURL)
		fmt.Printf("send this to %s; the URL is their credential\n", user)
	case "list":
		agents, err := hostagent.List(agentstate.DefaultPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "list:", err)
			os.Exit(1)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPARENT\tINBOX")
		for _, a := range agents {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, a.Parent, a.InboxChannel)
		}
		tw.Flush()
	default:
		workspaceUsage()
		os.Exit(2)
	}
}

func workspaceUsage() {
	fmt.Fprintln(os.Stderr, "usage: botbus workspace <create <name>|invite <user> --workspace <name>|list>")
}
