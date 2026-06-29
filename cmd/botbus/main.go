// botbus is a tiny line-oriented client for a botbus.ai channel.
// (Named "botbus" rather than the obvious "chat" because /usr/sbin/chat —
// macOS's legacy PPP chat-script tool — shadows it on $PATH.)
//
//	botbus                              # mint a fresh URL via new.botbus.ai
//	botbus https://<id>.botbus.ai/      # use this URL
//	botbus <id>                         # bare channel ID, https:// auto-added
//	botbus <id> --name NAME             # explicit display name
//
//	botbus --listen <id> --name NAME    # headless agent mode: print each
//	                                    # peer text message as "name: body"
//	                                    # on stdout, one per line. Designed
//	                                    # to be wrapped by a Claude Code
//	                                    # Monitor — peer messages arrive as
//	                                    # task-notifications, agent replies
//	                                    # via the botbus MCP gateway. Auto-
//	                                    # skips messages from --name (the
//	                                    # agent's own broadcasts) so they
//	                                    # don't trigger self-notifications.
//	                                    # MCP setup hints print to stderr.
//	                                    # Aliases: --monitor = --listen,
//	                                    # --skip = --name.
//	                                    # Add --filter NAME to only print
//	                                    # messages from a specific sender.
//
//	botbus --channel <id> --skip NAME   # Claude Code Channel mode: run as a
//	                                    # stdio MCP server that pushes each
//	                                    # peer message into the live session
//	                                    # as a notifications/claude/channel
//	                                    # event (no blocking, no stdout
//	                                    # scraping). Two-way via a `send`
//	                                    # tool. --from NAME aliases --filter.
//	                                    # The id may come from $BOTBUS_CHANNEL
//	                                    # instead of the positional arg (used by
//	                                    # the plugin's .mcp.json). See channel.go,
//	                                    # the plugin/ dir, and the README.
//
// File layout: ui.go owns the TUI (model/view/colors), ws.go owns the
// WebSocket loop, channel.go owns the MCP channel server, this file is just
// orchestration.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

const domain = "botbus.ai"

// DefaultRouterURL is the control-plane base for the routing fabric when neither
// a --router flag, the ROUTER_URL env, nor state.daemon.router_url is set. It
// points at the live router rather than a localhost address so a freshly-created
// agent's daemon registers/heartbeats against production out of the box — a
// localhost default produced empty/unreachable control bases and the daemon's
// "unsupported protocol scheme" / connection-refused spam. Shared by agent.go
// (realDeps) and daemon.go (resolveRouterURL); both are package main here.
const DefaultRouterURL = "https://router.botbus.ai"

// userAgent returns the User-Agent string this CLI sends on every HTTP and
// WebSocket request — used by new.botbus.ai's mint endpoint, the channel
// subdomain WS upgrades, and the proxy.golang.org update check. The "botbus"
// prefix is what the server's classifyUA matches to bucket us as classCLI;
// the version suffix is informational. Devel builds (no embedded version)
// still get the "botbus" prefix so classification works there too.
func userAgent() string {
	v := currentVersion()
	if v == "" {
		v = "devel"
	}
	return "botbus-cli/" + v
}

// resolveURL accepts any of:
//
//	(empty)              → mint a fresh URL via new.botbus.ai
//	https://X.botbus.ai/ → use as-is
//	X.botbus.ai          → prepend https://
//	X                    → prepend https://, append .botbus.ai
//
// The presence of a "." is the heuristic for "already a hostname" — channel
// IDs are base32 [0-9a-z minus iluo] and never contain a dot.
func resolveURL(arg string) (string, error) {
	if arg == "" {
		req, err := http.NewRequest(http.MethodGet, "https://new."+domain+"/", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", userAgent())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var b strings.Builder
		if _, err := io.Copy(&b, resp.Body); err != nil {
			return "", err
		}
		return strings.TrimSpace(b.String()), nil
	}
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		return strings.TrimRight(arg, "/") + "/", nil
	}
	arg = strings.TrimSuffix(arg, "/")
	if strings.Contains(arg, ".") {
		return "https://" + arg + "/", nil
	}
	return "https://" + arg + "." + domain + "/", nil
}

func hostFromURL(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

func wsURL(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// channelStreamURLs splits a channel URL into the two stream WebSocket URLs:
// the text endpoint ("/") and the audio endpoint ("/audio"). Server-side
// these are independent fan-out sets keyed by URL path.
func channelStreamURLs(httpURL string) (text, audio string) {
	base := strings.TrimRight(wsURL(httpURL), "/")
	return base + "/", base + "/audio"
}

// resolveName picks a chat name: $BOTBUS_NAME, then $USER, then anon-NNN.
// Stripped of ": " sequences (would break the message-parsing convention).
func resolveName() string {
	n := strings.TrimSpace(os.Getenv("BOTBUS_NAME"))
	if n == "" {
		n = strings.TrimSpace(os.Getenv("USER"))
	}
	if n == "" {
		var b [2]byte
		_, _ = rand.Read(b[:])
		n = fmt.Sprintf("anon-%03d", int(b[0])<<8|int(b[1])%900+100)
	}
	return strings.ReplaceAll(n, ": ", "_")
}

// cliArgs is the parsed shape of os.Args. Intentionally tiny: a single
// positional channel argument plus two flags. Anything unrecognized is
// silently dropped, matching the rest of the CLI's minimalist arg handling.
type cliArgs struct {
	channel     string
	monitor     bool
	channelMode bool   // --channel: run as a Claude Code Channel (stdio MCP server)
	name        string
	filter      string // --filter / --from: only surface messages from this sender
}

// parseArgs walks os.Args[1:] and returns the parsed flags. --name (alias
// --skip) takes a value; --listen (alias --monitor) is a toggle for headless
// agent mode. The first non-flag positional is the channel.
//
// --listen and --skip exist because the README documented those names while
// the code only accepted --monitor/--name; an agent following the README ran
// `botbus --listen <id>`, which was parsed as a positional, so headless mode
// never triggered and the binary tried to launch the TUI — dying with a TTY
// error under a non-interactive agent harness. Accepting both spellings makes
// every documented invocation work.
func parseArgs(argv []string) cliArgs {
	var a cliArgs
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--listen", "--monitor":
			a.monitor = true
		case "--channel":
			a.channelMode = true
		case "--name", "--skip":
			if i+1 < len(argv) {
				a.name = argv[i+1]
				i++
			}
		case "--filter", "--from":
			if i+1 < len(argv) {
				a.filter = argv[i+1]
				i++
			}
		default:
			if a.channel == "" {
				a.channel = argv[i]
			}
		}
	}
	return a
}

func main() {
	// Routing-fabric agent management is a distinct mode from the chat TUI.
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		agentCmd(os.Args[2:])
		return
	}
	// The multiplexing host daemon (inbox delivery + localhost MCP per agent).
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		daemonCmd(os.Args[2:])
		return
	}
	// Workspace lifecycle: an org-root anchor + invited members under it.
	if len(os.Args) > 1 && os.Args[1] == "workspace" {
		workspaceCmd(os.Args[2:])
		return
	}
	// Guided self-documenting onboarding (re-runnable).
	if len(os.Args) > 1 && os.Args[1] == "onboard" {
		runOnboard()
		return
	}
	// Fetch the role-aware briefing for a channel/agent URL and print it.
	if len(os.Args) > 1 && os.Args[1] == "attach" {
		attachCmd(os.Args[2:])
		return
	}

	args := parseArgs(os.Args[1:])

	// Bare `botbus` (no positional channel, not headless monitor mode) opens the
	// hierarchical operator console: first-run profile setup, the agent roster,
	// live dip-in chat, and onboarding. The positional-channel and --monitor
	// paths below are unchanged. (Previously bare `botbus` minted a fresh chat
	// channel; that behavior now lives only behind an explicit channel arg.)
	if args.channel == "" && !args.monitor && !args.channelMode {
		runConsole()
		return
	}

	// In --channel mode the channel id may arrive via $BOTBUS_CHANNEL instead of
	// a positional arg, so the plugin's static .mcp.json (`botbus --channel`)
	// needs no id baked in. Resolve it here and refuse to continue without one —
	// falling through to resolveURL("") would mint a fresh channel.
	if args.channelMode {
		id, ok := resolveChannelMode(args.channel, os.Getenv)
		if !ok {
			fmt.Fprintln(os.Stderr, "botbus --channel: pass a channel id or set BOTBUS_CHANNEL")
			os.Exit(1)
		}
		args.channel = id
	}

	// Resolve the user's display name: explicit --name beats env beats default.
	name := resolveName()
	if args.name != "" {
		name = strings.ReplaceAll(args.name, ": ", "_")
	}

	// In monitor and channel modes we're driven by another program (a Monitor
	// task, or Claude Code spawning us as a stdio MCP server); skip the
	// interactive update prompt and the stderr audio-hint so stdout stays a
	// clean stream and stdin stays unread.
	if !args.monitor && !args.channelMode {
		checkUpdateInteractive()
		if playerHint != "" {
			fmt.Fprint(os.Stderr, playerHint)
		}
	}

	u, err := resolveURL(args.channel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve:", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	recv := make(chan []byte, 64)
	audio := make(chan []byte, 8)
	send := make(chan []byte, 16)
	states := make(chan connState, 4)
	// seedCh is buffered so runWSText's one-shot /history seed never blocks —
	// the TUI model drains it; --monitor mode leaves it unread (and the ring
	// seed there just suppresses the startup last-40 replay so an agent wrapper
	// isn't flooded with stale messages).
	seedCh := make(chan seedMsg, 1)
	histBase := strings.TrimRight(u, "/") // channel HTTP origin for /history
	textURL, audioURL := channelStreamURLs(u)
	go runWSText(ctx, textURL, histBase, recv, send, states, seedCh)
	go runWSAudio(ctx, audioURL, histBase, audio)

	// Channel ID is the host minus ".botbus.ai", shared by both headless modes.
	channelID := strings.TrimSuffix(hostFromURL(u), ".botbus.ai")

	if args.channelMode {
		// Claude Code Channel: serve MCP over stdio and push peer messages as
		// claude/channel notifications. Blocks until stdin closes / signaled.
		if err := runChannel(ctx, recv, audio, states, send, name, args.filter, channelID); err != nil {
			fmt.Fprintln(os.Stderr, "channel:", err)
			os.Exit(1)
		}
		return
	}

	if args.monitor {
		fmt.Fprint(os.Stderr, monitorBanner(channelID, name))
		runMonitor(ctx, recv, audio, states, name, args.filter)
		return
	}

	go runAudio(ctx, audio)
	// fresh: the user ran `botbus` with no positional channel arg, so we
	// just minted a new channel via new.botbus.ai. The welcome popup uses
	// the "Your new private channel." copy when fresh and auto-shows
	// regardless of the per-channel marker file.
	fresh := args.channel == ""
	p := tea.NewProgram(newModel(hostFromURL(u), histBase, name, fresh, recv, states, send, seedCh), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
