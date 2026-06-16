// Package hostagent implements the host-side agent lifecycle for the botbus
// daemon/CLI: minting an agent's key + inbox channel, persisting it to the
// local state file, and registering it with the router's control API. It lives
// in an importable package (not cmd/botbus's main) so both the CLI and the
// daemon — and tests — can drive it.
package hostagent

import (
	"context"
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/wire"
)

// Deps are the injectable collaborators for Create (real in the CLI, faked in
// tests).
type Deps struct {
	Hub       hubclient.HubClient
	Control   *control.Client
	StatePath string
	MintKey   func() string
}

// CreateOpts are the user-supplied fields for a new agent.
type CreateOpts struct {
	ID        string
	Focus     string
	Mode      string
	Parent    string
	ModelTier string
}

// Batch defaults match the router's batcher defaults (see botbus router).
const (
	defaultBatchMS    = 3000
	defaultBatchN     = 5
	defaultBatchBytes = 20480
)

// Create mints a key + inbox channel, persists the agent to local state, and
// registers it with the router. It is idempotent at the registry (re-register
// replays desired state) but refuses to clobber an existing local entry.
func Create(ctx context.Context, d Deps, o CreateOpts) (agentstate.Agent, error) {
	if o.ID == "" {
		return agentstate.Agent{}, fmt.Errorf("agent id is required")
	}
	if o.Mode == "" {
		o.Mode = "session"
	}

	s, err := agentstate.Load(d.StatePath)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("load state: %w", err)
	}
	if _, exists := s.Get(o.ID); exists {
		return agentstate.Agent{}, fmt.Errorf("agent %q already exists locally", o.ID)
	}

	inbox, err := d.Hub.MintChannel(ctx)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("mint inbox channel: %w", err)
	}

	a := agentstate.Agent{
		ID:           o.ID,
		Key:          d.MintKey(),
		Name:         o.ID,
		InboxChannel: inbox,
		Focus:        o.Focus,
		Parent:       o.Parent,
		Mode:         o.Mode,
		BatchMS:      defaultBatchMS,
		BatchN:       defaultBatchN,
		BatchBytes:   defaultBatchBytes,
		ModelTier:    o.ModelTier,
	}

	s.Upsert(a)
	if err := agentstate.Save(d.StatePath, s); err != nil {
		return agentstate.Agent{}, fmt.Errorf("save state: %w", err)
	}

	if err := d.Control.Register(ctx, a.ID, a.Key, specOf(a)); err != nil {
		return agentstate.Agent{}, fmt.Errorf("register with router: %w", err)
	}
	return a, nil
}

// List returns the locally-registered agents.
func List(statePath string) ([]agentstate.Agent, error) {
	s, err := agentstate.Load(statePath)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	return s.Agents, nil
}

// Remove deletes an agent from local state. The registry entry is left for the
// daemon/operator to reap (presence simply expires); this removes only the
// local identity record so the host stops managing the agent.
func Remove(statePath, id string) error {
	s, err := agentstate.Load(statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !s.Remove(id) {
		return fmt.Errorf("agent %q not found locally", id)
	}
	if err := agentstate.Save(statePath, s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// specOf maps a local agent entry to the control-API register body.
func specOf(a agentstate.Agent) wire.AgentSpec {
	return wire.AgentSpec{
		Name: a.Name, InboxChannel: a.InboxChannel, Focus: a.Focus,
		Interest: a.Interest, Parent: a.Parent, Mode: a.Mode,
		BatchMS: a.BatchMS, BatchN: a.BatchN, BatchBytes: a.BatchBytes,
		ModelTier: a.ModelTier,
	}
}
