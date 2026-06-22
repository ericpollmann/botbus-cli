package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/mark3labs/mcp-go/mcp"
)

// TestInboxDeliversToNextTool ties the inbound path together: a router-shaped
// batch injected on the hub surfaces, unwrapped, through the agent's MCP `next`.
func TestInboxDeliversToNextTool(t *testing.T) {
	fake := hubclient.NewFake()
	rt := newRuntime("myth-compiler", 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runInbox(ctx, rt, fake, "inbox-c", "", func(string) {})
	time.Sleep(30 * time.Millisecond)

	fake.Inject("inbox-c", makeBatch(t, "c1",
		envelope.Envelope{ID: "m1", From: "eric", Body: "build please"},
	))

	// Build a minimal Daemon with the runtime pre-wired so ReadInbox resolves.
	st := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out"},
		Agents: []agentstate.Agent{{ID: "myth-compiler", Key: "key-xyz", Name: "myth-compiler", InboxChannel: "inbox-c"}},
	}
	d := &Daemon{state: st, hub: fake, runtimes: map[string]*AgentRuntime{"myth-compiler": rt}}
	ag := &agentMCP{ops: d, agentID: "myth-compiler", from: "myth-compiler"}

	deadline := time.After(2 * time.Second)
	for {
		res, err := ag.toolNext(ctx, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: "next", Arguments: map[string]any{"timeout_seconds": 1}},
		})
		if err != nil {
			t.Fatalf("toolNext: %v", err)
		}
		if txt := callText(t, res); strings.Contains(txt, "m1") && strings.Contains(txt, "build please") {
			return
		}
		select {
		case <-deadline:
			t.Fatal("routed batch never surfaced via next")
		default:
		}
	}
}

// TestRunServesMCPAndStops verifies Daemon.Run brings up the per-agent MCP
// endpoint on the configured localhost addr and shuts everything down cleanly
// when ctx is cancelled.
func TestRunServesMCPAndStops(t *testing.T) {
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ctl.Close()

	// Grab a free localhost port for the MCP listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	heartbeatEvery = 50 * time.Millisecond
	defer func() { heartbeatEvery = 30 * time.Second }()

	st := &agentstate.State{
		Daemon: agentstate.Daemon{RouterURL: ctl.URL, OutboundChannel: "out", MCPAddr: addr},
		Agents: []agentstate.Agent{{ID: "myth-compiler", Key: "key-xyz", InboxChannel: "inbox-c"}},
	}
	d := New(st, filepath.Join(t.TempDir(), "state.json"), hubclient.NewFake())

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- d.Run(ctx) }()

	// Wait for the listener, then POST to the agent endpoint — it must be routed
	// (not a ServeMux 404). A POST returns promptly (unlike an SSE GET).
	var status int
	deadline := time.After(2 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/a/key-xyz", strings.NewReader("{}"))
		resp, derr := (&http.Client{Timeout: time.Second}).Do(req)
		if derr == nil {
			status = resp.StatusCode
			resp.Body.Close()
			break
		}
		select {
		case <-deadline:
			t.Fatalf("MCP listener never came up: %v", derr)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if status == http.StatusNotFound {
		t.Fatal("agent MCP endpoint should be routed, got 404")
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
