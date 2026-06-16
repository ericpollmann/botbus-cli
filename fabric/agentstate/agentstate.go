// Package agentstate owns the host-side durable state file for the botbus
// daemon (default ~/.botbus/state.json, mode 0600). This file is canonical for
// agent identity (id, key, inbox channel) and config; the router's Redis is
// canonical only for presence. It lets the daemon re-register idempotently
// after a Redis flush, instance migration, or laptop-side reinstall.
package agentstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ericpollmann/botbus-proto/filter"
)

// Daemon holds host-wide connection config shared by all local agents.
type Daemon struct {
	RouterURL string `json:"router_url"`
	HubBase   string `json:"hub_base"`
	HubDomain string `json:"hub_domain"`
}

// Agent is one locally-managed fabric participant. Key and Cursor are secrets/
// state that never leave this file except as auth headers / resume tokens.
type Agent struct {
	ID           string        `json:"id"`
	Key          string        `json:"key"`
	Name         string        `json:"name,omitempty"`
	InboxChannel string        `json:"inbox_channel"`
	Focus        string        `json:"focus,omitempty"`
	Interest     string        `json:"interest,omitempty"`
	Parent       string        `json:"parent,omitempty"`
	Mode         string        `json:"mode,omitempty"`
	BatchMS      int           `json:"batch_ms,omitempty"`
	BatchN       int           `json:"batch_n,omitempty"`
	BatchBytes   int           `json:"batch_bytes,omitempty"`
	ModelTier    string        `json:"model_tier,omitempty"`
	Cursor       string        `json:"cursor,omitempty"`
	Filters      []filter.Rule `json:"filters,omitempty"`
}

// State is the full contents of the local state file.
type State struct {
	Daemon Daemon  `json:"daemon"`
	Agents []Agent `json:"agents"`
}

// DefaultPath returns $BOTBUS_STATE if set, else ~/.botbus/state.json.
func DefaultPath() string {
	if p := os.Getenv("BOTBUS_STATE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".botbus/state.json"
	}
	return filepath.Join(home, ".botbus", "state.json")
}

// Load reads the state file. A missing file yields an empty State (not an error).
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Save writes the state file atomically (temp + rename) with mode 0600,
// creating the parent directory if needed.
func Save(path string, s *State) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get returns the agent with the given id and whether it was found.
func (s *State) Get(id string) (Agent, bool) {
	for _, a := range s.Agents {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// Upsert inserts the agent, or replaces the existing entry with the same id.
func (s *State) Upsert(a Agent) {
	for i := range s.Agents {
		if s.Agents[i].ID == a.ID {
			s.Agents[i] = a
			return
		}
	}
	s.Agents = append(s.Agents, a)
}

// Remove deletes the agent by id, reporting whether one was removed.
func (s *State) Remove(id string) bool {
	for i := range s.Agents {
		if s.Agents[i].ID == id {
			s.Agents = append(s.Agents[:i], s.Agents[i+1:]...)
			return true
		}
	}
	return false
}

// SetCursor loads, updates the inbox resume cursor for one agent, and re-saves
// atomically. Returns an error if the agent id is unknown. Callers that advance
// the cursor on every frame should debounce writes themselves.
func SetCursor(path, id, cursor string) error {
	s, err := Load(path)
	if err != nil {
		return err
	}
	a, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("agentstate: unknown agent %q", id)
	}
	a.Cursor = cursor
	s.Upsert(a)
	return Save(path, s)
}
