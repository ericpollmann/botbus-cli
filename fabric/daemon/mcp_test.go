package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/wire"
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

// fakeOps records calls so we can assert the tools route through Ops.
type fakeOps struct {
	sentFrom    string
	sentArgs    SendArgs
	readID      string
	sendErr     error
	readErr     error
}

func (f *fakeOps) Roster(_ context.Context) ([]wire.AgentNode, error) { return nil, nil }
func (f *fakeOps) CreateChild(_ context.Context, _, _ string) (agentstate.Agent, ConnectInstructions, error) {
	return agentstate.Agent{}, ConnectInstructions{}, nil
}
func (f *fakeOps) Send(_ context.Context, from string, args SendArgs) error {
	f.sentFrom, f.sentArgs = from, args
	return f.sendErr
}
func (f *fakeOps) ReadInbox(_ context.Context, id string, _ int) (string, error) {
	f.readID = id
	return "[]", f.readErr
}
func (f *fakeOps) EnsureRoot(_ context.Context) (agentstate.Agent, error) {
	return agentstate.Agent{}, nil
}
func (f *fakeOps) Addr() string { return "127.0.0.1:8765" }

func TestToolSendRoutesThroughOps(t *testing.T) {
	f := &fakeOps{}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"body": "hello", "to": "bob", "kind": "dm", "subject": "sum"}
	if _, err := ag.toolSend(context.Background(), req); err != nil {
		t.Fatalf("toolSend: %v", err)
	}
	if f.sentFrom != "alice" {
		t.Fatalf("from not forwarded: got %q want alice", f.sentFrom)
	}
	if f.sentArgs.Body != "hello" {
		t.Fatalf("body not forwarded: got %q want hello", f.sentArgs.Body)
	}
	if len(f.sentArgs.To) != 1 || f.sentArgs.To[0] != "bob" {
		t.Fatalf("to not forwarded: got %v want [bob]", f.sentArgs.To)
	}
	if f.sentArgs.Subject != "sum" {
		t.Fatalf("subject not forwarded: got %q want sum", f.sentArgs.Subject)
	}
}

func TestToolNextRoutesThroughOps(t *testing.T) {
	f := &fakeOps{}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	if _, err := ag.toolNext(context.Background(), mcp.CallToolRequest{}); err != nil {
		t.Fatalf("toolNext: %v", err)
	}
	if f.readID != "a1" {
		t.Fatalf("ReadInbox not called with agentID, got %q", f.readID)
	}
}

func TestToolSendReturnsSent(t *testing.T) {
	f := &fakeOps{}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	// No "body" arg — GetString returns "" which is valid; test the "sent" result.
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"body": "hi"}
	res, err := ag.toolSend(context.Background(), req)
	if err != nil {
		t.Fatalf("toolSend unexpected error: %v", err)
	}
	if txt := callText(t, res); txt != "sent" {
		t.Fatalf("expected 'sent', got %q", txt)
	}
}

func TestToolNextTimeoutCap(t *testing.T) {
	f := &fakeOps{}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"timeout_seconds": 999}
	res, err := ag.toolNext(context.Background(), req)
	if err != nil {
		t.Fatalf("toolNext: %v", err)
	}
	// fakeOps.ReadInbox returns "[]", so result should be "[]"
	if txt := callText(t, res); txt != "[]" {
		t.Fatalf("expected '[]', got %q", txt)
	}
}

func TestToolNextErrorReturnsToolError(t *testing.T) {
	f := &fakeOps{readErr: errors.New("inbox unavailable")}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	res, err := ag.toolNext(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("toolNext: unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected tool error result, got: %+v", res)
	}
}

func TestToolSendErrorReturnsToolError(t *testing.T) {
	f := &fakeOps{sendErr: errors.New("publish failed")}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"body": "hello"}
	res, err := ag.toolSend(context.Background(), req)
	if err != nil {
		t.Fatalf("toolSend: unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected tool error result, got: %+v", res)
	}
}

func TestSplitComma(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b , c", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{"", nil},
		{" , ", nil},
	}
	for _, tc := range cases {
		got := splitComma(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("splitComma(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("splitComma(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
