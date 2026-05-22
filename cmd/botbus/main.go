// botbus is a tiny line-oriented client for a botbus.ai channel.
// (Named "botbus" rather than the obvious "chat" because /usr/sbin/chat —
// macOS's legacy PPP chat-script tool — shadows it on $PATH.)
//
//	botbus                              # mint a fresh URL via new.botbus.ai
//	botbus https://<id>.botbus.ai/      # use this URL
//	botbus <id>                         # bare channel ID, https:// auto-added
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
	"strings"

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

func main() {
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	u, err := resolveURL(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recv := make(chan []byte, 64)
	send := make(chan []byte, 16)
	states := make(chan connState, 4)
	go runWS(ctx, wsURL(u), recv, send, states)

	p := tea.NewProgram(newModel(hostFromURL(u), resolveName(), recv, states, send), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
