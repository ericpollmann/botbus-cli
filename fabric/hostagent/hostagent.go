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

// CreateOpts are the user-supplied fields for a new agent. Name is the human
// handle used for @mention/to: addressing; the opaque registration id is minted
// by the router, not chosen here.
type CreateOpts struct {
	Name      string
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
	if o.Name == "" {
		return agentstate.Agent{}, fmt.Errorf("agent name is required")
	}
	if o.Mode == "" {
		o.Mode = "session"
	}

	s, err := agentstate.Load(d.StatePath)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("load state: %w", err)
	}
	for _, ex := range s.Agents {
		if ex.Name == o.Name {
			return agentstate.Agent{}, fmt.Errorf("agent named %q already exists locally", o.Name)
		}
	}

	// The router mints the opaque, unguessable registration id (signed with the
	// deployment secret); the human name is only an addressing alias.
	id, err := d.Control.Mint(ctx)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("mint agent id: %w", err)
	}
	inbox, err := d.Hub.MintChannel(ctx)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("mint inbox channel: %w", err)
	}

	a := agentstate.Agent{
		ID:           id,
		Key:          d.MintKey(),
		Name:         o.Name,
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

// CreateRoot creates the workspace root — the first agent, with no parent.
func CreateRoot(ctx context.Context, d Deps) (agentstate.Agent, error) {
	return Create(ctx, d, CreateOpts{Name: "root"})
}

// EnsureRoot returns the workspace root, creating it on first run and reusing it
// on subsequent runs. It exists because Create persists to local state BEFORE
// Control.Register: a Register failure during first-run (e.g. router down)
// leaves a local "root" while the caller's profile.Save never ran, so a naive
// re-run of CreateRoot would hit "agent named root already exists locally" and
// wedge forever. EnsureRoot instead reuses any existing local root and just
// re-registers it (Register is idempotent). If the re-register still fails the
// existing local root is left intact so a later run succeeds once the router is
// reachable — and no second root is ever minted.
func EnsureRoot(ctx context.Context, d Deps) (agentstate.Agent, error) {
	existing, ok, err := GetByName(d.StatePath, "root")
	if err != nil {
		return agentstate.Agent{}, err
	}
	if ok {
		if rerr := d.Control.Register(ctx, existing.ID, existing.Key, specOf(existing)); rerr != nil {
			return agentstate.Agent{}, fmt.Errorf("re-register existing root: %w", rerr)
		}
		return existing, nil
	}
	return CreateRoot(ctx, d)
}

// GetByName returns the locally-registered agent with the given name (the human
// addressing alias, not the opaque id) and whether one was found.
func GetByName(statePath, name string) (agentstate.Agent, bool, error) {
	s, err := agentstate.Load(statePath)
	if err != nil {
		return agentstate.Agent{}, false, fmt.Errorf("load state: %w", err)
	}
	for _, a := range s.Agents {
		if a.Name == name {
			return a, true, nil
		}
	}
	return agentstate.Agent{}, false, nil
}

// List returns the locally-registered agents.
func List(statePath string) ([]agentstate.Agent, error) {
	s, err := agentstate.Load(statePath)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	return s.Agents, nil
}

// Remove deregisters an agent from the router AND deletes it from local state.
//
// The router deregister is best-effort: the local identity record is ALWAYS
// removed (so the host stops managing the agent) even when the router can't be
// reached or rejects the key. The returned routerErr reports a best-effort
// router failure (the registry entry then lingers until an operator/admin reaps
// it); err is non-nil only when the agent isn't known locally or local state
// couldn't be loaded/saved. The agent's key is read from local state before
// removal so it can authenticate the deregister call.
func Remove(ctx context.Context, d Deps, id string) (routerErr error, err error) {
	s, err := agentstate.Load(d.StatePath)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	a, ok := s.Get(id)
	if !ok {
		return nil, fmt.Errorf("agent %q not found locally", id)
	}
	// Best-effort: deregister from the router before dropping local state, using
	// the agent's bound key. A failure here (router down, key rotated, entry
	// already reaped) must NOT block the local removal below.
	if d.Control != nil {
		routerErr = d.Control.Deregister(ctx, a.ID, a.Key)
	}
	s.Remove(id) // ok == true, so this always removes
	// Removing the last managed agent legitimately leaves an empty list, so opt
	// past Save's empty-clobber guard.
	if err := agentstate.Save(d.StatePath, s, agentstate.AllowEmpty()); err != nil {
		return routerErr, fmt.Errorf("save state: %w", err)
	}
	return routerErr, nil
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
