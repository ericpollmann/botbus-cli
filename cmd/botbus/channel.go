package main

// channel.go — Claude Code Channel mode (`botbus --channel`).
//
// Unlike --monitor (which prints "name: body" to stdout for a Claude Code
// Monitor task to scrape), this mode runs as an MCP server over stdio that
// Claude Code spawns directly. It declares the experimental "claude/channel"
// capability, so each incoming peer message is pushed to the live session as a
// notifications/claude/channel JSON-RPC notification — injected into the
// conversation as <channel source="botbus" name="…" channel="…">body</channel>
// without blocking a turn (the old next()/monitor approaches either froze the
// turn on a long-poll or relied on stdout line-scraping).
//
// The server owns a *dynamic* set of channel subscriptions. It mirrors the
// cloud gateway's tool vocabulary (subscribe/unsubscribe/send/set_name/list/
// new_channel) MINUS next() — there is no pull in push mode. subscribe()
// starts pushing a channel's messages; unsubscribe() stops; both can be called
// live, so a running session can bind/unbind channels without a restart.
//
// Requires Claude Code v2.1.80+ and registration as a channel (see README).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// channelInstructions is added to Claude's system prompt at initialize. It
// tells Claude what the injected <channel> events look like and how to act,
// per the channel contract's recommendation to ship handling guidance.
const channelInstructions = "Live chat from botbus channels arrives as " +
	"<channel source=\"botbus\" name=\"SENDER\" channel=\"CHANNEL_ID\">body</channel>. " +
	"The `channel` attribute says which channel the message came from. To reply, " +
	"call the botbus `send` tool with that channel id and your text (reply body " +
	"only — don't echo the attributes). Use `subscribe`/`unsubscribe` to add or " +
	"drop channels live, `list` to see active ones, `new_channel` to mint one."

// parseChannelSeeds splits the positional arg and $BOTBUS_CHANNEL into an
// ordered, de-duplicated list of channel ids/hosts/URLs. Tokens are separated
// by commas or whitespace. The list may be empty — a channel session can start
// with no subscriptions and add them live via the subscribe tool.
func parseChannelSeeds(positional, env string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, p := range strings.FieldsFunc(s, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}) {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	add(positional)
	add(env)
	return out
}

// chanSub is one live channel subscription: a cancel func that tears down its
// WebSocket pump + fan-out goroutines, and the send channel its replies go to.
type chanSub struct {
	cancel context.CancelFunc
	send   chan []byte
}

// chanManager owns the dynamic set of channel subscriptions for one Claude Code
// channel session. Each subscription is an independent WS listener whose frames
// are pushed to the session as notifications/claude/channel events tagged with
// meta.channel, so Claude can tell channels apart and reply to the right one.
type chanManager struct {
	ctx    context.Context
	srv    *server.MCPServer
	mu     sync.Mutex
	subs   map[string]*chanSub
	name   string // outgoing display name + own-echo skip filter
	filter string // optional: only inject messages from this sender
}

// runChannelManager builds the stdio MCP channel server, seeds it with the
// given channels, and serves until stdin closes or the process is signaled.
func runChannelManager(ctx context.Context, seeds []string, name, filter string) error {
	m := &chanManager{
		ctx:    ctx,
		subs:   map[string]*chanSub{},
		name:   name,
		filter: filter,
	}
	s := server.NewMCPServer("botbus", currentVersion(),
		server.WithExperimental(map[string]any{"claude/channel": map[string]any{}}),
		server.WithInstructions(channelInstructions),
		server.WithToolCapabilities(false),
	)
	m.srv = s

	s.AddTool(mcp.NewTool("subscribe",
		mcp.WithDescription("Start pushing a channel's messages into this session as <channel> events (no polling). Idempotent."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel id, host (id.botbus.ai), or full URL.")),
	), m.toolSubscribe)

	s.AddTool(mcp.NewTool("unsubscribe",
		mcp.WithDescription("Stop pushing a channel's messages into this session."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("A currently-subscribed channel id, host, or URL.")),
	), m.toolUnsubscribe)

	s.AddTool(mcp.NewTool("send",
		mcp.WithDescription("Send a message to a subscribed channel."),
		mcp.WithString("channel", mcp.Required(), mcp.Description("Channel to send to (must be subscribed).")),
		mcp.WithString("text", mcp.Required(), mcp.Description("Message body to post.")),
	), m.toolSend)

	s.AddTool(mcp.NewTool("set_name",
		mcp.WithDescription("Set the outgoing display name. Also filtered from inbound, so you aren't notified of your own messages."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Display name. \": \" is rewritten to \"_\".")),
	), m.toolSetName)

	s.AddTool(mcp.NewTool("list",
		mcp.WithDescription("List currently-subscribed channels and the active outgoing name."),
	), m.toolList)

	s.AddTool(mcp.NewTool("new_channel",
		mcp.WithDescription("Mint a fresh botbus channel URL. Returns {url, channel_id}. Subscribe to it to start receiving."),
	), m.toolNewChannel)

	// Seed initial subscriptions. Failures are logged to stderr (not stdout —
	// stdout carries MCP protocol framing) and don't abort startup.
	for _, ch := range seeds {
		if _, err := m.subscribe(ch); err != nil {
			fmt.Fprintln(os.Stderr, "channel: seed subscribe", ch, "failed:", err)
		}
	}

	// Serve MCP over stdio; blocks until stdin closes or the process is
	// signaled. ServeStdio installs its own SIGTERM/SIGINT handling.
	return server.ServeStdio(s)
}

// subscribe adds a live listener for ch (id/host/URL). It is idempotent: a
// channel already subscribed returns its canonical id without starting a second
// listener. Never pass "" — that would mint a fresh channel via resolveURL.
func (m *chanManager) subscribe(ch string) (string, error) {
	if strings.TrimSpace(ch) == "" {
		return "", fmt.Errorf("channel is required")
	}
	u, err := resolveURL(ch)
	if err != nil {
		return "", err
	}
	cid := strings.TrimSuffix(hostFromURL(u), ".botbus.ai")

	m.mu.Lock()
	if _, ok := m.subs[cid]; ok {
		m.mu.Unlock()
		return cid, nil // already subscribed
	}
	cctx, cancel := context.WithCancel(m.ctx)
	send := make(chan []byte, 16)
	m.subs[cid] = &chanSub{cancel: cancel, send: send}
	m.mu.Unlock()

	recv := make(chan []byte, 64)
	states := make(chan connState, 8)
	seedCh := make(chan seedMsg, 1) // backlog seed (suppresses last-40 replay)
	histBase := strings.TrimRight(u, "/")
	textURL, _ := channelStreamURLs(u)
	go runWSText(cctx, textURL, histBase, recv, send, states, seedCh)
	go func() { // drain connection-state breadcrumbs (never to stdout)
		for range states {
		}
	}()
	go m.listen(cctx, cid, recv)
	return cid, nil
}

// listen fans a channel's incoming frames into claude/channel notifications,
// tagged with the channel id, until recv closes (the subscription's ctx was
// canceled). Own broadcasts and, when a filter is set, other senders are
// dropped before notifying.
func (m *chanManager) listen(ctx context.Context, cid string, recv <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-recv:
			if !ok {
				return
			}
			from, body, _, named := parseMsgWithID(msg)
			if !named {
				continue // raw non-protocol frame
			}
			m.mu.Lock()
			name, filter := m.name, m.filter
			m.mu.Unlock()
			if from == name {
				continue // our own echo
			}
			if filter != "" && from != filter {
				continue
			}
			m.srv.SendNotificationToAllClients("notifications/claude/channel",
				map[string]any{
					"content": body,
					"meta": map[string]any{
						"name":    from,
						"channel": cid,
					},
				})
		}
	}
}

// unsubscribe tears down a channel's listener. Returns the canonical id.
func (m *chanManager) unsubscribe(ch string) (string, error) {
	u, err := resolveURL(ch)
	if err != nil {
		return "", err
	}
	cid := strings.TrimSuffix(hostFromURL(u), ".botbus.ai")
	m.mu.Lock()
	sub, ok := m.subs[cid]
	if ok {
		delete(m.subs, cid)
	}
	m.mu.Unlock()
	if !ok {
		return cid, fmt.Errorf("not subscribed: %s", cid)
	}
	sub.cancel()
	return cid, nil
}

func (m *chanManager) toolSubscribe(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cid, err := m.subscribe(req.GetString("channel", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("subscribed: " + cid), nil
}

func (m *chanManager) toolUnsubscribe(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cid, err := m.unsubscribe(req.GetString("channel", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("unsubscribed: " + cid), nil
}

func (m *chanManager) toolSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch := req.GetString("channel", "")
	text := req.GetString("text", "")
	if ch == "" || text == "" {
		return mcp.NewToolResultError("channel and text are required"), nil
	}
	u, err := resolveURL(ch)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	cid := strings.TrimSuffix(hostFromURL(u), ".botbus.ai")
	m.mu.Lock()
	sub, ok := m.subs[cid]
	name := m.name
	m.mu.Unlock()
	if !ok {
		return mcp.NewToolResultError("not subscribed: " + cid), nil
	}
	select {
	case sub.send <- []byte(name + ": " + text):
		return mcp.NewToolResultText("sent"), nil
	case <-ctx.Done():
		return mcp.NewToolResultError("send canceled"), nil
	}
}

func (m *chanManager) toolSetName(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	n := strings.ReplaceAll(req.GetString("name", ""), ": ", "_")
	if n == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	m.mu.Lock()
	m.name = n
	m.mu.Unlock()
	return mcp.NewToolResultText("name: " + n), nil
}

func (m *chanManager) toolList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	m.mu.Lock()
	chans := make([]string, 0, len(m.subs))
	for cid := range m.subs {
		chans = append(chans, cid)
	}
	name := m.name
	m.mu.Unlock()
	out, _ := json.Marshal(map[string]any{"name": name, "channels": chans})
	return mcp.NewToolResultText(string(out)), nil
}

func (m *chanManager) toolNewChannel(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	u, err := resolveURL("") // mints via new.botbus.ai
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	cid := strings.TrimSuffix(hostFromURL(u), ".botbus.ai")
	out, _ := json.Marshal(map[string]any{"url": u, "channel_id": cid})
	return mcp.NewToolResultText(string(out)), nil
}
