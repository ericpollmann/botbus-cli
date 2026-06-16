package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/mark3labs/mcp-go/mcp"
)

func callText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] not TextContent: %T", res.Content[0])
	}
	return tc.Text
}

func TestToolNextReturnsEnvelopes(t *testing.T) {
	rt := newRuntime("a", 100)
	rt.enqueue(envelope.Envelope{ID: "m1", From: "eric", Body: "build"})
	ag := &agentMCP{rt: rt, hub: hubclient.NewFake(), outbound: "out", from: "a"}

	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "next", Arguments: map[string]any{"timeout_seconds": 1}}}
	res, err := ag.toolNext(context.Background(), req)
	if err != nil {
		t.Fatalf("toolNext: %v", err)
	}
	text := callText(t, res)
	if !strings.Contains(text, "m1") || !strings.Contains(text, "build") {
		t.Fatalf("next text missing envelope: %s", text)
	}
}

func TestToolSendPublishes(t *testing.T) {
	fake := hubclient.NewFake()
	ag := &agentMCP{rt: newRuntime("a", 100), hub: fake, outbound: "out", from: "myth-compiler"}
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "send", Arguments: map[string]any{
		"body": "hello", "to": "myth-boss, council", "kind": "dm",
	}}}
	res, err := ag.toolSend(context.Background(), req)
	if err != nil {
		t.Fatalf("toolSend: %v", err)
	}
	if got := callText(t, res); got != "sent" {
		t.Fatalf("send result = %q", got)
	}
	pubs := fake.Published("out")
	if len(pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(pubs))
	}
	e, _ := envelope.Decode([]byte(strings.TrimPrefix(pubs[0], "myth-compiler: ")))
	if e.Kind != "dm" || len(e.To) != 2 || e.To[0] != "myth-boss" || e.To[1] != "council" {
		t.Fatalf("bad envelope: %+v", e)
	}
}

func TestBuildAgentHandlerNotNil(t *testing.T) {
	ag := &agentMCP{rt: newRuntime("a", 100), hub: hubclient.NewFake(), outbound: "out", from: "a"}
	if buildAgentHandler(ag) == nil {
		t.Fatal("handler is nil")
	}
}
