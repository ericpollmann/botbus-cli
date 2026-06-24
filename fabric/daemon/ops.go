package daemon

import (
	"context"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/wire"
)

// ConnectInstructions tells the operator how to attach an agent to its inbox.
// MCPCommand/MCPEndpoint are preferred when the runtime hosts a local MCP
// endpoint; ChannelURL is the raw curl-recipe fallback.
type ConnectInstructions struct {
	MCPCommand  string // `claude mcp add --transport http <name> http://<addr>/a/<key>`
	MCPEndpoint string // http://<addr>/a/<key>
	ChannelURL  string // https://<inbox>.<domain>/
}

// Ops is the single local-agent operation surface every face (TUI, MCP, and a
// future HTTP face) calls. Implemented by *Daemon so there is exactly one
// implementation of each operation.
type Ops interface {
	Roster(ctx context.Context) ([]wire.AgentNode, error)
	CreateChild(ctx context.Context, name, focus string) (agentstate.Agent, ConnectInstructions, error)
	Send(ctx context.Context, fromAgent string, args SendArgs) error
	ReadInbox(ctx context.Context, agentID string, timeoutSec int) (string, error)
	EnsureRoot(ctx context.Context) (agentstate.Agent, error)
	// Addr is the local MCP listen address (host:port) used to build connect
	// instructions. *Daemon already implements it.
	Addr() string
	// Remove deregisters an agent from the router and deletes it from local
	// state. routerErr is a best-effort router-side failure (local removal still
	// happened); err is non-nil only when the agent isn't known locally or state
	// I/O failed.
	Remove(ctx context.Context, id string) (routerErr error, err error)
}
