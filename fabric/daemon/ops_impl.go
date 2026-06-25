package daemon

import (
	"context"
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/console"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/wire"
)

// Compile-time assertion: *Daemon satisfies the Ops interface.
var _ Ops = (*Daemon)(nil)

// root returns the operator's root agent (id + capability key). Resolution
// order: (1) loaded profile, (2) in-memory state, (3) local state file.
func (d *Daemon) root() (agentstate.Agent, error) {
	if d.profile != nil && d.profile.Root.ID != "" {
		return agentstate.Agent{
			ID: d.profile.Root.ID, Key: d.profile.Root.Key,
			Name: "root", InboxChannel: d.profile.Root.InboxChannel,
		}, nil
	}
	// Check the in-memory state first (avoids a disk read and works when
	// statePath is empty, e.g. in tests that pass State directly).
	for _, a := range d.state.Agents {
		if a.Name == "root" {
			return a, nil
		}
	}
	a, ok, err := hostagent.GetByName(d.statePath, "root")
	if err != nil {
		return agentstate.Agent{}, err
	}
	if !ok {
		return agentstate.Agent{}, fmt.Errorf("no root agent — run first-run setup")
	}
	return a, nil
}

// hostDeps builds the hostagent collaborators from the runtime's own fields.
func (d *Daemon) hostDeps() hostagent.Deps {
	return hostagent.Deps{
		Hub: d.hub, Control: d.control, StatePath: d.statePath, MintKey: d.mintKey,
	}
}

// CreateChild registers a sub-agent under root (mint id + inbox channel +
// register with Parent + seed welcome) and returns MCP-first connect
// instructions. It does NOT spawn a process (see spec Follow-ups).
func (d *Daemon) CreateChild(ctx context.Context, name, focus string) (agentstate.Agent, ConnectInstructions, error) {
	r, err := d.root()
	if err != nil {
		return agentstate.Agent{}, ConnectInstructions{}, err
	}
	child, err := hostagent.Create(ctx, d.hostDeps(), hostagent.CreateOpts{
		Name: name, Focus: focus, Parent: r.ID,
	})
	if err != nil {
		return agentstate.Agent{}, ConnectInstructions{}, fmt.Errorf("create child: %w", err)
	}
	welcome := console.RenderWelcome(child.Name, focus, "root", d.profile)
	if err := console.SeedWelcome(ctx, d.hub, child.InboxChannel, welcome); err != nil {
		return agentstate.Agent{}, ConnectInstructions{}, fmt.Errorf("seed welcome: %w", err)
	}
	endpoint := fmt.Sprintf("http://%s/a/%s", d.Addr(), child.Key)
	return child, ConnectInstructions{
		MCPCommand:  fmt.Sprintf("claude mcp add --transport http %s %s", child.Name, endpoint),
		MCPEndpoint: endpoint,
		ChannelURL:  fmt.Sprintf("https://%s.%s/", child.InboxChannel, d.domain),
	}, nil
}

// Roster returns the agent tree (parent links + liveness) as the root.
func (d *Daemon) Roster(ctx context.Context) ([]wire.AgentNode, error) {
	r, err := d.root()
	if err != nil {
		return nil, err
	}
	return d.control.Roster(ctx, r.ID, r.Key)
}

// Send publishes a message as fromAgent to the daemon's outbound source channel
// (the router routes it). args carries the full wire fields (body, to, kind,
// subject, scope); kind defaults to chat when empty.
func (d *Daemon) Send(ctx context.Context, fromAgent string, args SendArgs) error {
	return Send(ctx, d.hub, d.state.Daemon.OutboundChannel, fromAgent, args)
}

// Remove deregisters + deletes a managed agent by id (the op behind the
// console's `d` remove key).
func (d *Daemon) Remove(ctx context.Context, id string) (routerErr error, err error) {
	return hostagent.Remove(ctx, d.hostDeps(), id)
}

// ReadInbox long-polls one agent's inbox queue (the op behind MCP `next`),
// returning the queued envelopes as a JSON array string. Errors if agentID is
// not a managed runtime.
func (d *Daemon) ReadInbox(ctx context.Context, agentID string, timeoutSec int) (string, error) {
	rt, ok := d.runtimes[agentID]
	if !ok {
		return "", fmt.Errorf("unknown agent id %q", agentID)
	}
	return Next(ctx, rt, timeoutSec), nil
}
