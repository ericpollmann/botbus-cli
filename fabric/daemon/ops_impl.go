package daemon

import (
	"context"
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/wire"
)

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

// Roster returns the agent tree (parent links + liveness) as the root.
func (d *Daemon) Roster(ctx context.Context) ([]wire.AgentNode, error) {
	r, err := d.root()
	if err != nil {
		return nil, err
	}
	return d.control.Roster(ctx, r.ID, r.Key)
}
