// botbus-mcp is an MCP gateway that lets MCP-aware agents (Claude Code,
// Claude Desktop, claude.ai with custom MCP, etc.) join a botbus.ai
// channel without holding their own WebSocket.
//
// Tools exposed:
//
//	new_channel             mint a fresh channel URL via new.botbus.ai
//	set_name <name>         set the display name for outgoing messages
//	subscribe <channel>     open a long-lived WS to the channel; begin buffering
//	next <channel> [timeout] block until the next message arrives (or timeout)
//	send <channel> <text>   send "name: text" on the channel
//	unsubscribe <channel>   close the WS, stop buffering
//	list                    show currently subscribed channels
//
// "<channel>" accepts a bare channel ID, a host (id.botbus.ai), or a
// full URL (https://id.botbus.ai/) — anything pointing at one channel.
//
// Run as stdio MCP server. Wire it into Claude Desktop with:
//
//	{
//	  "mcpServers": { "botbus": { "command": "botbus-mcp" } }
//	}
//
// or Claude Code:
//
//	claude mcp add botbus botbus-mcp
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const domain = "botbus.ai"

type subscription struct {
	id     string
	ws     *websocket.Conn
	recv   chan string
	ctx    context.Context
	cancel context.CancelFunc
}

type gateway struct {
	mu   sync.Mutex
	subs map[string]*subscription
	name string
}

func defaultName() string {
	for _, env := range []string{"BOTBUS_NAME", "USER"} {
		if n := strings.TrimSpace(os.Getenv(env)); n != "" {
			return strings.ReplaceAll(n, ": ", "_")
		}
	}
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "anon-" + hex.EncodeToString(b[:])
}

// normalize accepts URL, host, or bare ID; returns the bare channel ID.
func normalize(channel string) (string, error) {
	s := strings.TrimSpace(channel)
	for _, p := range []string{"https://", "http://", "wss://", "ws://"} {
		s = strings.TrimPrefix(s, p)
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "."+domain)
	if s == "" {
		return "", fmt.Errorf("empty channel")
	}
	if strings.Contains(s, ".") || strings.Contains(s, "/") {
		return "", fmt.Errorf("invalid channel %q (host doesn't end in .%s)", channel, domain)
	}
	return s, nil
}

func wsURL(id string) string   { return "wss://" + id + "." + domain + "/" }
func httpURL(id string) string { return "https://" + id + "." + domain + "/" }

func argString(req mcp.CallToolRequest, name string) string {
	if v, ok := req.GetArguments()[name].(string); ok {
		return v
	}
	return ""
}
func argFloat(req mcp.CallToolRequest, name string) (float64, bool) {
	v, ok := req.GetArguments()[name].(float64)
	return v, ok
}

func textResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(b)), nil
}

// ---- tools ----

func (g *gateway) newChannel(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := http.Get("https://new." + domain + "/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return nil, err
	}
	url := strings.TrimSpace(string(body))
	id, err := normalize(url)
	if err != nil {
		return nil, err
	}
	return textResult(map[string]string{
		"url": url, "channel_id": id,
		"host":      id + "." + domain,
		"web_chat":  url,
		"web_voice": url + "voice",
	})
}

func (g *gateway) setName(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := strings.ReplaceAll(argString(req, "name"), ": ", "_")
	if name == "" {
		return mcp.NewToolResultError("name required"), nil
	}
	g.mu.Lock()
	g.name = name
	g.mu.Unlock()
	return textResult(map[string]string{"name": name})
}

func (g *gateway) subscribe(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := normalize(argString(req, "channel"))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	g.mu.Lock()
	if _, exists := g.subs[id]; exists {
		g.mu.Unlock()
		return textResult(map[string]string{"channel_id": id, "status": "already-subscribed"})
	}
	g.mu.Unlock()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	ws, _, err := websocket.Dial(dialCtx, wsURL(id), nil)
	dialCancel()
	if err != nil {
		return mcp.NewToolResultError("dial failed: " + err.Error()), nil
	}
	ws.SetReadLimit(64 * 1024)

	// First frame after the server's 200ms anti-scan delay should be "ok".
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, okMsg, err := ws.Read(hsCtx)
	hsCancel()
	if err != nil || string(okMsg) != "ok" {
		ws.CloseNow()
		return mcp.NewToolResultError(fmt.Sprintf("handshake failed: err=%v msg=%q", err, okMsg)), nil
	}

	subCtx, cancel := context.WithCancel(context.Background())
	sub := &subscription{
		id:     id,
		ws:     ws,
		recv:   make(chan string, 256),
		ctx:    subCtx,
		cancel: cancel,
	}
	// Reader: pump WS frames into sub.recv until cancel.
	go func() {
		defer close(sub.recv)
		for {
			_, m, err := ws.Read(subCtx)
			if err != nil {
				return
			}
			select {
			case sub.recv <- string(m):
			case <-subCtx.Done():
				return
			}
		}
	}()

	g.mu.Lock()
	g.subs[id] = sub
	g.mu.Unlock()

	return textResult(map[string]string{
		"channel_id": id,
		"host":       id + "." + domain,
		"status":     "subscribed",
	})
}

func splitNameBody(msg string) (name, body string) {
	if i := strings.Index(msg, ": "); i > 0 {
		return msg[:i], msg[i+2:]
	}
	return "", msg
}

func (g *gateway) next(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := normalize(argString(req, "channel"))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	timeoutSec := 30.0
	if t, ok := argFloat(req, "timeout_seconds"); ok {
		timeoutSec = t
	}
	if timeoutSec > 300 {
		timeoutSec = 300
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}

	g.mu.Lock()
	sub, ok := g.subs[id]
	g.mu.Unlock()
	if !ok {
		return mcp.NewToolResultError("not subscribed to " + id + " — call subscribe first"), nil
	}

	select {
	case msg, open := <-sub.recv:
		if !open {
			g.mu.Lock()
			delete(g.subs, id)
			g.mu.Unlock()
			return textResult(map[string]any{"channel_id": id, "status": "disconnected"})
		}
		name, body := splitNameBody(msg)
		return textResult(map[string]any{
			"channel_id": id, "name": name, "body": body, "raw": msg,
		})
	case <-time.After(time.Duration(timeoutSec * float64(time.Second))):
		return textResult(map[string]any{"channel_id": id, "status": "timeout"})
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *gateway) send(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := normalize(argString(req, "channel"))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	text := argString(req, "text")
	if text == "" {
		return mcp.NewToolResultError("text required"), nil
	}

	g.mu.Lock()
	sub, hasSub := g.subs[id]
	name := g.name
	if n := strings.ReplaceAll(argString(req, "name"), ": ", "_"); n != "" {
		name = n
	}
	g.mu.Unlock()

	full := []byte(name + ": " + text)

	// Prefer the live WS if we have one — server excludes the sender from
	// broadcast that way, so we don't see our own message via next(). If
	// not subscribed, fall back to POST.
	if hasSub {
		wctx, wc := context.WithTimeout(sub.ctx, 5*time.Second)
		err := sub.ws.Write(wctx, websocket.MessageBinary, full)
		wc()
		if err != nil {
			return mcp.NewToolResultError("ws write failed: " + err.Error()), nil
		}
		return textResult(map[string]any{"sent": true, "via": "ws", "name": name, "channel_id": id})
	}
	resp, err := http.Post(httpURL(id), "text/plain", strings.NewReader(string(full)))
	if err != nil {
		return mcp.NewToolResultError("post failed: " + err.Error()), nil
	}
	resp.Body.Close()
	return textResult(map[string]any{"sent": true, "via": "post", "name": name, "channel_id": id})
}

func (g *gateway) unsubscribe(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := normalize(argString(req, "channel"))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	g.mu.Lock()
	sub, ok := g.subs[id]
	if ok {
		delete(g.subs, id)
	}
	g.mu.Unlock()
	if !ok {
		return textResult(map[string]string{"channel_id": id, "status": "not-subscribed"})
	}
	sub.cancel()
	sub.ws.CloseNow()
	return textResult(map[string]string{"channel_id": id, "status": "unsubscribed"})
}

func (g *gateway) list(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	g.mu.Lock()
	ids := make([]string, 0, len(g.subs))
	for id := range g.subs {
		ids = append(ids, id)
	}
	name := g.name
	g.mu.Unlock()
	return textResult(map[string]any{"name": name, "subscriptions": ids})
}

func main() {
	g := &gateway{
		subs: map[string]*subscription{},
		name: defaultName(),
	}

	s := server.NewMCPServer("botbus", "0.1.0", server.WithToolCapabilities(false))

	s.AddTool(mcp.NewTool("new_channel",
		mcp.WithDescription(
			"Mint a fresh botbus.ai channel URL. Returns {url, channel_id, host, web_chat, web_voice}. "+
				"The URL is the secret — anyone who has it can read and write the channel."),
	), g.newChannel)

	s.AddTool(mcp.NewTool("set_name",
		mcp.WithDescription("Set the display name used for outgoing messages. Persists for the gateway process."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Display name. Cannot contain \": \" (auto-rewritten to \"_\").")),
	), g.setName)

	s.AddTool(mcp.NewTool("subscribe",
		mcp.WithDescription(
			"Open a WebSocket to a channel and begin buffering incoming messages. "+
				"Idempotent; call once per channel you want to follow. "+
				"After subscribing, call next() to retrieve messages."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID, host (id.botbus.ai), or full URL.")),
	), g.subscribe)

	s.AddTool(mcp.NewTool("next",
		mcp.WithDescription(
			"Block until the next message arrives on a subscribed channel, or until timeout. "+
				"Returns {channel_id, name, body, raw} on a message, or {status: \"timeout\"} on timeout. "+
				"Call repeatedly to drain. You will not see your own messages via this tool."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID, host, or URL.")),
		mcp.WithNumber("timeout_seconds", mcp.Description("Default 30, max 300, min 1.")),
	), g.next)

	s.AddTool(mcp.NewTool("send",
		mcp.WithDescription(
			"Send \"name: text\" to a channel. Uses the live WebSocket if subscribed (avoids self-echo), "+
				"falls back to HTTP POST if not. Returns {sent, via, name, channel_id}."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID, host, or URL.")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Message body. Will be prefixed with current name and \": \".")),
		mcp.WithString("name", mcp.Description("Override the current name for this send only.")),
	), g.send)

	s.AddTool(mcp.NewTool("unsubscribe",
		mcp.WithDescription("Close the WebSocket for a channel and stop buffering."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel ID, host, or URL.")),
	), g.unsubscribe)

	s.AddTool(mcp.NewTool("list",
		mcp.WithDescription("List currently subscribed channels and the active outgoing name."),
	), g.list)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintln(os.Stderr, "botbus-mcp:", err)
		os.Exit(1)
	}
}
