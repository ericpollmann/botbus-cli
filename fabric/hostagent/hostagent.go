// Package hostagent implements the host-side agent lifecycle for the botbus
// daemon/CLI: minting an agent's key + inbox channel, persisting it to the
// local state file, and registering it with the router's control API. It lives
// in an importable package (not cmd/botbus's main) so both the CLI and the
// daemon — and tests — can drive it.
package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/nacl/box"

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
	E2E       bool // when true, a 32-byte ed25519 signing seed is generated for this agent
}

// newSignSeed generates a fresh 32-byte ed25519 signing seed (from the private
// key's seed bytes). Panics only on a catastrophic OS random-source failure
// (as GenerateKey would).
func newSignSeed() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate sign seed: %w", err)
	}
	return priv.Seed(), nil
}

// newEncKey generates a fresh 32-byte X25519 private key for NaCl sealed-box
// decryption. The returned slice is priv[:] — a copy of the array on the heap.
func newEncKey() ([]byte, error) {
	_, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate enc key: %w", err)
	}
	return priv[:], nil
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
	if o.E2E {
		seed, err := newSignSeed()
		if err != nil {
			return agentstate.Agent{}, fmt.Errorf("generate sign seed: %w", err)
		}
		a.SignSeed = seed
		a.EncPriv, err = newEncKey()
		if err != nil {
			return agentstate.Agent{}, fmt.Errorf("generate enc key: %w", err)
		}
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

// UpdateFields are the optionally-changed fields; a nil pointer means "leave
// as-is", while a non-nil pointer (including to the empty string) sets the
// field — so --focus "" explicitly clears Focus.
type UpdateFields struct {
	Focus, Interest, Parent, Mode, ModelTier *string
}

// Update loads the local agent by name, applies the non-nil fields, persists
// the change to local state, then RE-REGISTERS it with the router (Register is
// idempotent, so this replays the desired spec). Identity (ID/Key/InboxChannel)
// is never changed. It errors if no local agent has that name. Order matches
// Create's "local is canonical" stance: local state is saved before the router
// call, so a Register failure keeps the local change and surfaces the error.
func Update(ctx context.Context, d Deps, name string, f UpdateFields) (agentstate.Agent, error) {
	a, ok, err := GetByName(d.StatePath, name)
	if err != nil {
		return agentstate.Agent{}, err
	}
	if !ok {
		return agentstate.Agent{}, fmt.Errorf("no local agent named %q", name)
	}

	if f.Focus != nil {
		a.Focus = *f.Focus
	}
	if f.Interest != nil {
		a.Interest = *f.Interest
	}
	if f.Parent != nil {
		a.Parent = *f.Parent
	}
	if f.Mode != nil {
		a.Mode = *f.Mode
	}
	if f.ModelTier != nil {
		a.ModelTier = *f.ModelTier
	}

	s, err := agentstate.Load(d.StatePath)
	if err != nil {
		return agentstate.Agent{}, fmt.Errorf("load state: %w", err)
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
