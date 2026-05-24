// botbus is a tiny line-oriented client for a botbus.ai channel.
// (Named "botbus" rather than the obvious "chat" because /usr/sbin/chat —
// macOS's legacy PPP chat-script tool — shadows it on $PATH.)
//
//	botbus                              # mint a fresh URL via new.botbus.ai
//	botbus https://<id>.botbus.ai/      # use this URL
//	botbus <id>                         # bare channel ID, https:// auto-added
//
//	botbus --listen <id> [--skip NAME …]   # headless listener mode: print
//	                                       # each peer text message as
//	                                       # "name: body" on stdout, one per
//	                                       # line. Designed for agent / Monitor
//	                                       # wake-up loops. --skip filters
//	                                       # specific senders (e.g. your own
//	                                       # name) so you don't notify on
//	                                       # your own broadcasts. Audio
//	                                       # frames are dropped silently;
//	                                       # the update prompt is skipped.
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

// parseArgs walks os.Args[1:] and returns (channel, listen, skipSet). The
// shape is intentionally tiny — one positional channel argument plus a
// --listen toggle and any number of --skip NAME pairs. Anything else is
// ignored, mirroring the rest of the CLI's minimalist arg handling.
func parseArgs(argv []string) (channel string, listen bool, skip map[string]struct{}) {
	skip = map[string]struct{}{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "--listen":
			listen = true
		case "--skip":
			if i+1 < len(argv) {
				skip[argv[i+1]] = struct{}{}
				i++
			}
		default:
			if channel == "" {
				channel = a
			}
		}
	}
	return
}

// runListen pumps incoming text frames to stdout one per line as "name: body".
// Designed for agent wake-up loops: a Monitor wraps this command and gets
// notified on each peer message. Audio frames are dropped silently — agents
// don't (yet) handle audio. State changes log to stderr so the wrapping
// Monitor doesn't see them as channel content.
func runListen(ctx context.Context, recv, audio <-chan []byte, states <-chan connState, skip map[string]struct{}) {
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
			name, body, named := parseMsg(m)
			if !named {
				continue // raw, non-protocol — agents only act on "name: body"
			}
			if _, drop := skip[name]; drop {
				continue
			}
			fmt.Printf("%s: %s\n", name, body)
		}
	}
}

func main() {
	channel, listen, skip := parseArgs(os.Args[1:])

	// In listen mode we're being driven by another program (Monitor); skip
	// the interactive update prompt and the stderr audio-hint to keep
	// stdout/stderr clean and free of blocking reads.
	if !listen {
		checkUpdateInteractive()
		if playerHint != "" {
			fmt.Fprint(os.Stderr, playerHint)
		}
	}

	u, err := resolveURL(channel)
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

	if listen {
		runListen(ctx, recv, audio, states, skip)
		return
	}

	go runAudio(ctx, audio)
	p := tea.NewProgram(newModel(hostFromURL(u), resolveName(), recv, states, send), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
