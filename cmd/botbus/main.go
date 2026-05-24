// botbus is a tiny line-oriented client for a botbus.ai channel.
// (Named "botbus" rather than the obvious "chat" because /usr/sbin/chat —
// macOS's legacy PPP chat-script tool — shadows it on $PATH.)
//
//	botbus                              # mint a fresh URL via new.botbus.ai
//	botbus https://<id>.botbus.ai/      # use this URL
//	botbus <id>                         # bare channel ID, https:// auto-added
//	botbus <id> --name NAME             # explicit display name
//
//	botbus --monitor <id> --name NAME   # headless agent mode: print each
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
//
// File layout: ui.go owns the TUI (model/view/colors), ws.go owns the
// WebSocket loop, this file is just orchestration.
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
		resp, err := http.Get("https://new." + domain + "/")
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
	channel string
	monitor bool
	name    string
}

// parseArgs walks os.Args[1:] and returns the parsed flags. --name takes a
// value; --monitor is a toggle. The first non-flag positional is the channel.
func parseArgs(argv []string) cliArgs {
	var a cliArgs
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--monitor":
			a.monitor = true
		case "--name":
			if i+1 < len(argv) {
				a.name = argv[i+1]
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

// monitorBanner returns the stderr greeting printed once at monitor startup.
// It tells the wrapping agent which channel + name it's connected to and
// shows the exact MCP tool calls it should use to reply on this specific
// channel. Goes to stderr so stdout stays a clean stream of peer messages.
func monitorBanner(channelID, name string) string {
	return "" +
		"botbus monitor: connected to https://" + channelID + ".botbus.ai/ as \"" + name + "\"\n" +
		"\n" +
		"Each peer message arrives on stdout as \"name: body\" and triggers a\n" +
		"task-notification. Reply via the botbus MCP gateway:\n" +
		"\n" +
		"  mcp__botbus__set_name(name=\"" + name + "\")\n" +
		"  mcp__botbus__subscribe(channel=\"" + channelID + "\")\n" +
		"  mcp__botbus__send(channel=\"" + channelID + "\", text=\"…\")\n" +
		"\n" +
		"If the botbus MCP gateway isn't yet configured in your environment:\n" +
		"  claude mcp add --transport http botbus https://mcp.botbus.ai/mcp\n" +
		"\n"
}

// runMonitor pumps incoming text frames to stdout one per line as "name: body".
// Designed for agent wake-up loops: a Monitor wraps this command and gets
// notified on each peer message. The agent's own broadcasts (from `name`)
// are filtered so the agent doesn't notify on itself. Audio frames are
// dropped silently — agents don't (yet) handle audio. State changes log to
// stderr so the wrapping Monitor doesn't see them as channel content.
func runMonitor(ctx context.Context, recv, audio <-chan []byte, states <-chan connState, name string) {
	// Drain side channels so runWS never blocks on full buffers.
	go func() {
		for range audio {
		}
	}()
	go func() {
		for s := range states {
			switch s {
			case stConnecting:
				fmt.Fprintln(os.Stderr, "botbus: connecting…")
			case stConnected:
				fmt.Fprintln(os.Stderr, "botbus: connected")
			case stDown:
				fmt.Fprintln(os.Stderr, "botbus: disconnected, will retry")
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-recv:
			if !ok {
				return
			}
			from, body, named := parseMsg(m)
			if !named {
				continue // raw, non-protocol — agents only act on "name: body"
			}
			if from == name {
				continue // our own broadcast (cross-connection); skip
			}
			fmt.Printf("%s: %s\n", from, body)
		}
	}
}

func main() {
	args := parseArgs(os.Args[1:])

	// Resolve the user's display name: explicit --name beats env beats default.
	name := resolveName()
	if args.name != "" {
		name = strings.ReplaceAll(args.name, ": ", "_")
	}

	// In monitor mode we're driven by another program (Monitor wrapping us);
	// skip the interactive update prompt and the stderr audio-hint so stdout
	// stays a clean stream and stdin stays unread.
	if !args.monitor {
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
	go runWS(ctx, wsURL(u), recv, audio, send, states)

	if args.monitor {
		// Channel ID is the host minus ".botbus.ai" — strip it for display.
		channelID := strings.TrimSuffix(hostFromURL(u), ".botbus.ai")
		fmt.Fprint(os.Stderr, monitorBanner(channelID, name))
		runMonitor(ctx, recv, audio, states, name)
		return
	}

	go runAudio(ctx, audio)
	p := tea.NewProgram(newModel(hostFromURL(u), name, recv, states, send), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
