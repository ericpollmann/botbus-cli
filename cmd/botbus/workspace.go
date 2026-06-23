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
	"io"
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

// parseInviteArgs parses `<user> --workspace <name>`, accepting the positional
// user and the --workspace flag in ANY order. Go's flag.Parse stops at the first
// non-flag arg, so a naive parse of `invite ethan --workspace x` would leave
// --workspace unparsed (the bug this fixes); this interleaves flag-parsing with
// positional collection so either ordering works. ok is false unless exactly one
// positional (the user) and a non-empty workspace are present.
func parseInviteArgs(args []string) (user, ws string, ok bool) {
	fs := flag.NewFlagSet("workspace invite", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // workspaceUsage handles the user-facing message
	wsp := fs.String("workspace", "", "workspace name to invite into (required)")
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return "", "", false
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	if len(positionals) != 1 || positionals[0] == "" || *wsp == "" {
		return "", "", false
	}
	return positionals[0], *wsp, true
}

// setActiveWorkspace loads the state file, sets ActiveWorkspace to the given
// org-root id, and re-saves. The console scopes its roster to this subtree.
func setActiveWorkspace(statePath, orgRootID string) error {
	s, err := agentstate.Load(statePath)
	if err != nil {
		return err
	}
	s.ActiveWorkspace = orgRootID
	return agentstate.Save(statePath, s)
}

// workspaceUse switches the active workspace to the workspace named name,
// resolving it to its org-root agent id. It errors clearly (and changes
// nothing) if no such workspace exists locally.
func workspaceUse(statePath, name string) error {
	root, ok, err := hostagent.GetByName(statePath, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no workspace named %q — create it first", name)
	}
	return setActiveWorkspace(statePath, root.ID)
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
		deps := realDeps()
		a, err := workspaceCreate(ctx, deps, name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		// Creating a workspace makes it the active workspace.
		if err := setActiveWorkspace(deps.StatePath, a.ID); err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		fmt.Printf("created workspace %q\n  root id: %s\n  channel: https://%s.%s\n", a.Name, a.ID, a.InboxChannel, domain)
	case "invite":
		user, ws, ok := parseInviteArgs(args[1:])
		if !ok {
			workspaceUsage()
			os.Exit(2)
		}
		joinURL, err := workspaceInvite(ctx, realDeps(), user, ws)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invite:", err)
			os.Exit(1)
		}
		fmt.Println(joinURL)
		fmt.Printf("send this to %s; the URL is their credential\n", user)
	case "use":
		if len(args) < 2 || args[1] == "" {
			workspaceUsage()
			os.Exit(2)
		}
		name := args[1]
		if err := workspaceUse(realDeps().StatePath, name); err != nil {
			fmt.Fprintln(os.Stderr, "use:", err)
			os.Exit(1)
		}
		fmt.Printf("active workspace is now %q\n", name)
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
	fmt.Fprintln(os.Stderr, "usage: botbus workspace <create <name>|invite <user> --workspace <name>|use <name>|list>")
}
