package daemon

import (
	"context"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// agentMCP binds one agent's Ops surface for its MCP tools.
type agentMCP struct {
	ops     Ops
	agentID string
	from    string
}

// buildMCPServer registers the next/send tools for one agent and returns the
// MCP server (so callers can wrap it in a streamable handler with a chosen
// endpoint path).
func buildMCPServer(ag *agentMCP) *server.MCPServer {
	s := server.NewMCPServer("botbus-daemon", "0.1.0", server.WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("next",
		mcp.WithDescription("Long-poll this agent's inbox; returns a JSON array of envelopes (possibly empty on timeout)."),
		mcp.WithNumber("timeout_seconds", mcp.Description("Default 30, max 300.")),
	), ag.toolNext)
	s.AddTool(mcp.NewTool("send",
		mcp.WithDescription("Publish an outbound message; the daemon stamps id/ts/from."),
		mcp.WithString("body", mcp.Required()),
		mcp.WithString("to", mcp.Description("Comma-separated agent ids for direct address.")),
		mcp.WithString("kind", mcp.Description("chat|dm|task|escalate|status|review_request; default chat.")),
		mcp.WithString("subject", mcp.Description("Optional short summary.")),
	), ag.toolSend)
	return s
}

func (ag *agentMCP) toolNext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	timeout := req.GetInt("timeout_seconds", 30)
	if timeout > 300 {
		timeout = 300
	}
	out, err := ag.ops.ReadInbox(ctx, ag.agentID, timeout)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

func (ag *agentMCP) toolSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var to []string
	if s := req.GetString("to", ""); s != "" {
		to = splitComma(s)
	}
	if err := ag.ops.Send(ctx, ag.from, req.GetString("body", ""), to, req.GetString("kind", "")); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("sent"), nil
}

// splitComma splits "a, b ,c" into ["a","b","c"], trimming spaces and empties.
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newAgentHandler returns a streamable-HTTP MCP handler exposing next/send for
// one agent at the given endpoint path.
func newAgentHandler(ag *agentMCP, path string) http.Handler {
	return server.NewStreamableHTTPServer(buildMCPServer(ag), server.WithEndpointPath(path))
}
