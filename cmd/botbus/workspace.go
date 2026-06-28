package main

// workspace.go — the `botbus workspace <create|invite|list>` subcommands. A
// workspace is anchored by an org-root agent (Parent==""); invited humans become
// members parented to that root. The real logic lives in small functions that
// take a hostagent.Deps so tests inject fakes + a temp state path; workspaceCmd
// just wires realDeps() and prints. Dispatched only when argv[1] == "workspace".

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"text/tabwriter"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
)

// workspaceCreate mints the org-root anchor for a workspace: an agent with no
// parent. Members are later invited under it. When e2e is true the workspace
// gets an encryption key set and the org-root agent gets a signing seed.
func workspaceCreate(ctx context.Context, d hostagent.Deps, name string, e2e bool) (agentstate.Agent, error) {
	root, err := hostagent.Create(ctx, d, hostagent.CreateOpts{Name: name, E2E: e2e})
	if err != nil {
		return agentstate.Agent{}, err
	}
	if !e2e {
		return root, nil
	}
	// Mint workspace key material: 32-byte symmetric key, 32-byte HKDF salt,
	// and an admin Ed25519 keypair (pinned for device-set signing).
	var key, salt [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return agentstate.Agent{}, fmt.Errorf("generate workspace key: %w", err)
	}
	if _, err := rand.Read(salt[:]); err != nil {
		return agentstate.Agent{}, fmt.Errorf("generate workspace salt: %w", err)
	}
	adminPub, adminPrivKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("generate admin keypair: %w", err)
	}
	s, err := agentstate.Load(d.StatePath)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("load state: %w", err)
	}
	rosterChannel, err := d.Hub.MintChannel(ctx)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("mint roster channel: %w", err)
	}
	waitingRoomChannel, err := d.Hub.MintChannel(ctx)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("mint waiting room channel: %w", err)
	}
	s.Workspaces = append(s.Workspaces, agentstate.Workspace{
		RootID:      root.ID,
		E2E:         true,
		Epoch:       1,
		Key:         key[:],
		Salt:        salt[:],
		AdminPub:    []byte(adminPub),
		AdminPriv:   adminPrivKey.Seed(),
		Roster:      rosterChannel,
		WaitingRoom: waitingRoomChannel,
	})
	if err := agentstate.Save(d.StatePath, s); err != nil {
		return agentstate.Agent{}, fmt.Errorf("save workspace: %w", err)
	}
	return root, nil
}

// workspaceInvite finds the workspace's org-root by name and creates a member
// agent parented to it, returning the member's join URL. The join URL embeds the
// member's inbox channel as the host and carries the (url-escaped) user as a
// query param — it IS the member's credential. For e2e workspaces the member
// also receives a signing seed.
func workspaceInvite(ctx context.Context, d hostagent.Deps, user, wsName string) (joinURL string, err error) {
	root, ok, err := hostagent.GetByName(d.StatePath, wsName)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no workspace named %q — create it first", wsName)
	}
	// Propagate E2E flag for member agents in e2e workspaces so they get a
	// signing seed too.
	s, err := agentstate.Load(d.StatePath)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}
	ws, isE2E := s.WorkspaceFor(root.ID)
	memberE2E := isE2E && ws.E2E
	member, err := hostagent.Create(ctx, d, hostagent.CreateOpts{Name: user, Parent: root.ID, E2E: memberE2E})
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

// workspacePending loads the state file, resolves the active (or named)
// workspace, and returns one formatted line per pending join request:
// "<reqId>  <name>  <SAS>  <parentIntent>". wsName == "" ⇒ active workspace.
func workspacePending(statePath, wsName string) (string, error) {
	s, err := agentstate.Load(statePath)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}
	rootID := s.ActiveWorkspace
	if wsName != "" {
		root, ok, err := hostagent.GetByName(statePath, wsName)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no workspace named %q — create it first", wsName)
		}
		rootID = root.ID
	}
	var ws *agentstate.Workspace
	for i := range s.Workspaces {
		if s.Workspaces[i].RootID == rootID {
			ws = &s.Workspaces[i]
			break
		}
	}
	if ws == nil {
		return "", fmt.Errorf("no e2e workspace for root %q", rootID)
	}
	if len(ws.Pending) == 0 {
		return "", nil
	}
	var out string
	for _, p := range ws.Pending {
		sas := daemon.SASFingerprint(p.SignPub, p.EncPub)
		out += fmt.Sprintf("%s\t%s\t%s\t%s\n", p.ReqID, p.Name, sas, p.ParentIntent)
	}
	return out, nil
}

// workspaceAdmit admits a pending join request identified by reqID in the
// active (or named) workspace. It reconstructs an in-process daemon, hydrates
// the trust graph, calls AdmitJoinRequest, removes the request from Pending,
// and saves state.
func workspaceAdmit(ctx context.Context, d hostagent.Deps, wsName, reqID string) error {
	st, err := agentstate.Load(d.StatePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	rootID := st.ActiveWorkspace
	if wsName != "" {
		root, ok, err := hostagent.GetByName(d.StatePath, wsName)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no workspace named %q — create it first", wsName)
		}
		rootID = root.ID
	}
	var ws *agentstate.Workspace
	for i := range st.Workspaces {
		if st.Workspaces[i].RootID == rootID {
			ws = &st.Workspaces[i]
			break
		}
	}
	if ws == nil {
		return fmt.Errorf("no e2e workspace for root %q", rootID)
	}
	// Find the pending request.
	var pending *agentstate.PendingJoin
	for i := range ws.Pending {
		if ws.Pending[i].ReqID == reqID {
			pending = &ws.Pending[i]
			break
		}
	}
	if pending == nil {
		return fmt.Errorf("no pending request with id %q", reqID)
	}
	dm := daemon.NewRuntime(daemon.Config{State: st, StatePath: d.StatePath, Hub: d.Hub})
	dm.HydrateWorkspaceTrust(ws)
	req := daemon.JoinRequest{
		ReqID:        pending.ReqID,
		Name:         pending.Name,
		ParentIntent: pending.ParentIntent,
		SignPub:      pending.SignPub,
		EncPub:       pending.EncPub,
	}
	if _, err := dm.AdmitJoinRequest(ctx, ws, req); err != nil {
		return fmt.Errorf("admit: %w", err)
	}
	// Remove the admitted entry from Pending (in-place filter).
	filtered := ws.Pending[:0]
	for _, p := range ws.Pending {
		if p.ReqID != reqID {
			filtered = append(filtered, p)
		}
	}
	ws.Pending = filtered
	if err := agentstate.Save(d.StatePath, st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
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
		// Parse: create <name> [--e2e]
		fs := flag.NewFlagSet("workspace create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		e2eFlag := fs.Bool("e2e", false, "create an end-to-end encrypted workspace")
		var positionals []string
		rest := args[1:]
		for {
			if err := fs.Parse(rest); err != nil {
				workspaceUsage()
				os.Exit(2)
			}
			rest = fs.Args()
			if len(rest) == 0 {
				break
			}
			positionals = append(positionals, rest[0])
			rest = rest[1:]
		}
		if len(positionals) != 1 || positionals[0] == "" {
			workspaceUsage()
			os.Exit(2)
		}
		name := positionals[0]
		deps := realDeps()
		a, err := workspaceCreate(ctx, deps, name, *e2eFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		// Creating a workspace makes it the active workspace.
		if err := setActiveWorkspace(deps.StatePath, a.ID); err != nil {
			fmt.Fprintln(os.Stderr, "create:", err)
			os.Exit(1)
		}
		if *e2eFlag {
			s, _ := agentstate.Load(deps.StatePath)
			var joinHandle string
			for _, ws := range s.Workspaces {
				if ws.RootID == a.ID {
					joinHandle = ws.WaitingRoom
					break
				}
			}
			fmt.Printf("created e2e workspace %q\n  root id: %s\n  channel: https://%s.%s\n  join handle: %s\n", a.Name, a.ID, a.InboxChannel, domain, joinHandle)
		} else {
			fmt.Printf("created workspace %q\n  root id: %s\n  channel: https://%s.%s\n", a.Name, a.ID, a.InboxChannel, domain)
		}
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
	case "pending":
		fs := flag.NewFlagSet("workspace pending", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		wsp := fs.String("workspace", "", "workspace name (default: active workspace)")
		if err := fs.Parse(args[1:]); err != nil {
			workspaceUsage()
			os.Exit(2)
		}
		out, err := workspacePending(agentstate.DefaultPath(), *wsp)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pending:", err)
			os.Exit(1)
		}
		if out == "" {
			fmt.Println("no pending requests")
		} else {
			fmt.Print(out)
		}
	case "admit":
		// Parse: admit <reqId> [--workspace <name>]
		fs := flag.NewFlagSet("workspace admit", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		wsp := fs.String("workspace", "", "workspace name (default: active workspace)")
		var positionals []string
		rest := args[1:]
		for {
			if err := fs.Parse(rest); err != nil {
				workspaceUsage()
				os.Exit(2)
			}
			rest = fs.Args()
			if len(rest) == 0 {
				break
			}
			positionals = append(positionals, rest[0])
			rest = rest[1:]
		}
		if len(positionals) != 1 || positionals[0] == "" {
			workspaceUsage()
			os.Exit(2)
		}
		reqID := positionals[0]
		if err := workspaceAdmit(ctx, realDeps(), *wsp, reqID); err != nil {
			fmt.Fprintln(os.Stderr, "admit:", err)
			os.Exit(1)
		}
		fmt.Printf("admitted %s\n", reqID)
	default:
		workspaceUsage()
		os.Exit(2)
	}
}

func workspaceUsage() {
	fmt.Fprintln(os.Stderr, "usage: botbus workspace <create <name> [--e2e]|invite <user> --workspace <name>|use <name>|list|pending [--workspace <name>]|admit <reqId> [--workspace <name>]>")
}
