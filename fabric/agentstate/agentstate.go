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
	RouterURL       string `json:"router_url"`
	HubBase         string `json:"hub_base"`
	HubDomain       string `json:"hub_domain"`
	OutboundChannel string `json:"outbound_channel,omitempty"` // source channel `send` publishes to
	MCPAddr         string `json:"mcp_addr,omitempty"`         // localhost MCP listen addr (default 127.0.0.1:8765)
}

// Agent is one locally-managed fabric participant. Key and Cursor are secrets/
// state that never leave this file (or its sibling 0600 *.bak backups) except
// as auth headers / resume tokens.
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
	SignSeed     []byte        `json:"signSeed,omitempty"` // 32-byte Ed25519 seed; deviceID == Agent.ID
	EncPriv      []byte        `json:"encPriv,omitempty"`  // 32-byte X25519 private key (e2e agents only)
}

// AnchorRef is a persisted record of an admitted remote anchor: the identity
// (Ed25519) the admin signs into the anchor set, and the X25519 encryption
// pubkey a rotated workspace key is wrapped to. Stored on the admin host only.
type AnchorRef struct {
	ID      string `json:"id"`
	SignPub []byte `json:"signPub"`
	EncPub  []byte `json:"encPub"`
}

// PendingJoin is a persisted copy of an inbound JoinRequest that has arrived
// on the waiting-room channel but has not yet been admitted by the admin.
// Kept in agentstate to avoid an import cycle with the daemon package.
type PendingJoin struct {
	ReqID        string `json:"reqId"`
	Name         string `json:"name"`
	ParentIntent string `json:"parentIntent,omitempty"`
	SignPub      []byte `json:"signPub"`
	EncPub       []byte `json:"encPub"`
}

// Workspace holds e2e encryption config for an org-root.
type Workspace struct {
	RootID      string `json:"rootId"`
	E2E         bool   `json:"e2e,omitempty"`
	Epoch       uint32 `json:"epoch,omitempty"`
	Key         []byte `json:"key,omitempty"`         // 32-byte symmetric workspace key
	Salt        []byte `json:"salt,omitempty"`        // per-epoch HKDF salt (Phase 3 uses)
	AdminPub    []byte `json:"adminPub,omitempty"`    // pinned admin Ed25519 pubkey
	AdminPriv   []byte `json:"adminPriv,omitempty"`   // admin Ed25519 private key (stored only on creator host)
	Roster      string `json:"roster,omitempty"`      // per-workspace roster channel id for cert distribution
	WaitingRoom string        `json:"waitingRoom,omitempty"` // channel where join requests arrive before admission
	Anchors     []AnchorRef   `json:"anchors,omitempty"`     // admitted remote anchors (admin host); source of truth for rekey re-wraps
	Pending     []PendingJoin `json:"pending,omitempty"`     // inbound join requests not yet admitted (admin host only)
}

// State is the full contents of the local state file.
type State struct {
	Daemon Daemon `json:"daemon"`
	// ActiveWorkspace is the org-root agent id of the operator's currently
	// selected workspace. The console scopes its roster to this subtree; empty
	// means "no active workspace — show everything".
	ActiveWorkspace string      `json:"active_workspace,omitempty"`
	Agents          []Agent     `json:"agents"`
	Workspaces      []Workspace `json:"workspaces,omitempty"`
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

// backupGenerations is how many prior versions of the state file are retained
// alongside the live file (state.json.bak, state.json.bak.1, ...). The agent
// capability keys live only in this file, so a wipe must stay recoverable.
const backupGenerations = 3

// saveOpts holds the resolved options for a Save call.
type saveOpts struct {
	allowEmpty bool
}

// SaveOption configures a Save call.
type SaveOption func(*saveOpts)

// AllowEmpty permits Save to write a State with no agents over a file that
// currently has agents. Without it, Save refuses such a write to guard against
// a downgraded or buggy binary wiping every agent's capability key. The one
// legitimate empty case is removing the last locally-managed agent, which must
// thread this option through.
func AllowEmpty() SaveOption {
	return func(o *saveOpts) { o.allowEmpty = true }
}

// Save writes the state file atomically (temp + rename) with mode 0600,
// creating the parent directory if needed. Before overwriting an existing
// file, its current contents are rotated into a small ring of backups so an
// accidental wipe — e.g. by a downgraded binary — stays recoverable. Save also
// refuses to replace a populated agent list with an empty one unless
// AllowEmpty is passed.
func Save(path string, s *State, opts ...SaveOption) error {
	var o saveOpts
	for _, fn := range opts {
		fn(&o)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	exists := err == nil

	if exists && len(s.Agents) == 0 && !o.allowEmpty {
		if n := agentCount(existing); n > 0 {
			return fmt.Errorf("agentstate: refusing to overwrite %d agent(s) in %s with an empty state (pass AllowEmpty to clear intentionally)", n, path)
		}
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if exists {
		if err := rotateBackups(path, existing); err != nil {
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

// agentCount reports how many agents the serialized state holds. Unparseable
// bytes report 0 so the empty-clobber guard never blocks on a prior file it
// cannot interpret.
func agentCount(b []byte) int {
	var s State
	if json.Unmarshal(b, &s) != nil {
		return 0
	}
	return len(s.Agents)
}

// backupName returns the filename for the given backup generation: generation
// 0 is the most recent (state.json.bak), higher numbers are older.
func backupName(path string, gen int) string {
	if gen == 0 {
		return path + ".bak"
	}
	return fmt.Sprintf("%s.bak.%d", path, gen)
}

// rotateBackups shifts the existing backups one generation older (dropping the
// oldest) and writes current as the freshest generation. current is the live
// file's contents captured before it is overwritten; the backup carries the
// same 0600 permissions because it holds the same secret keys.
func rotateBackups(path string, current []byte) error {
	for i := backupGenerations - 2; i >= 0; i-- {
		if err := os.Rename(backupName(path, i), backupName(path, i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.WriteFile(backupName(path, 0), current, 0o600)
}

// Clone returns a deep copy of the state. All slice fields are copied so
// mutation of the clone's Agents/Workspaces does not affect the original and
// vice-versa.
func (s *State) Clone() *State {
	c := *s
	c.Agents = append([]Agent(nil), s.Agents...)
	c.Workspaces = make([]Workspace, len(s.Workspaces))
	for i, ws := range s.Workspaces {
		w := ws
		w.Key = append([]byte(nil), ws.Key...)
		w.Salt = append([]byte(nil), ws.Salt...)
		w.AdminPub = append([]byte(nil), ws.AdminPub...)
		w.AdminPriv = append([]byte(nil), ws.AdminPriv...)
		w.Anchors = append([]AnchorRef(nil), ws.Anchors...)
		w.Pending = append([]PendingJoin(nil), ws.Pending...)
		c.Workspaces[i] = w
	}
	return &c
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

// AgentByID returns a pointer to the agent with the given id and whether it was found.
func (s *State) AgentByID(id string) (*Agent, bool) {
	for i := range s.Agents {
		if s.Agents[i].ID == id {
			return &s.Agents[i], true
		}
	}
	return nil, false
}

// WorkspaceRootID walks Parent links to the org-root (Parent==""). It is
// cycle-safe: it visits at most len(Agents) hops and returns "" on a cycle or
// a dangling parent, mirroring the server-side registry.RootAncestorID.
func (s *State) WorkspaceRootID(agentID string) string {
	cur, ok := s.AgentByID(agentID)
	if !ok {
		return ""
	}
	for hops := 0; hops <= len(s.Agents); hops++ {
		if cur.Parent == "" {
			return cur.ID
		}
		next, ok := s.AgentByID(cur.Parent)
		if !ok {
			return "" // dangling parent
		}
		cur = next
	}
	return "" // cycle
}

// WorkspaceFor looks up the workspace for the given agent by walking to the root.
func (s *State) WorkspaceFor(agentID string) (*Workspace, bool) {
	root := s.WorkspaceRootID(agentID)
	if root == "" {
		return nil, false
	}
	for i := range s.Workspaces {
		if s.Workspaces[i].RootID == root {
			return &s.Workspaces[i], true
		}
	}
	return nil, false
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
