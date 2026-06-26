package daemon

import (
	"context"
	"crypto/ed25519"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/sync/errgroup"
)

// DefaultMCPAddr is the localhost MCP listen address when state doesn't set one.
const DefaultMCPAddr = "127.0.0.1:8765"

// Daemon multiplexes a host's agents: one inbox subscription + runtime +
// presence loop + MCP endpoint each, over a single process and hub connection
// set.
type Daemon struct {
	// state.Daemon, statePath, hub, control, mintKey, domain are written once at
	// construction and only read afterwards, so they're safe to read without mu.
	// Only state.Agents (the slice contents) and the maps below are mutated at
	// runtime and require mu.
	state     *agentstate.State
	statePath string
	hub       hubclient.HubClient
	runtimes  map[string]*AgentRuntime

	control *control.Client
	profile *profile.Profile
	mintKey func() string
	domain  string

	mu       sync.Mutex                    // guards state.Agents, runtimes, handlers, cancels, serving, runCtx, ctl, counters
	handlers map[string]http.Handler       // capability key -> cached streamable MCP handler
	cancels  map[string]context.CancelFunc // agentID -> loop canceller (set only while serving)
	runCtx   context.Context               // parent ctx for attached agents' loops (set in RunOn)
	ctl      *control.Client               // resolved control client (set in RunOn)
	serving  bool                          // true while RunOn is active
	devices  *deviceSet
	replay   *replayWindow
	counters map[string]uint64 // keyed by "deviceID|channelID|epoch"; lazy-init in nextCounter
}

// Config is the full set of runtime collaborators.
type Config struct {
	State     *agentstate.State
	StatePath string
	Hub       hubclient.HubClient
	Control   *control.Client
	Profile   *profile.Profile
	MintKey   func() string
	Domain    string
}

// NewRuntime builds the single local-agent runtime from its collaborators.
func NewRuntime(c Config) *Daemon {
	rts := make(map[string]*AgentRuntime, len(c.State.Agents))
	for _, a := range c.State.Agents {
		rts[a.ID] = newRuntime(a.ID, 1000)
	}
	return &Daemon{
		state: c.State, statePath: c.StatePath, hub: c.Hub, runtimes: rts,
		control: c.Control, profile: c.Profile, mintKey: c.MintKey, domain: c.Domain,
		handlers: make(map[string]http.Handler),
		cancels:  make(map[string]context.CancelFunc),
		devices:  newDeviceSet(),
		replay:   newReplayWindow(),
	}
}

// New is the back-compat constructor (inbox/MCP only; control built lazily in Run).
func New(state *agentstate.State, statePath string, hub hubclient.HubClient) *Daemon {
	return NewRuntime(Config{State: state, StatePath: statePath, Hub: hub})
}

// seedDeviceFor registers the agent's ed25519 pubkey in the local device set
// when the agent has a valid SignSeed and belongs to an e2e workspace. This
// allows same-host sibling agents to verify each other's signatures without a
// roster channel. Safe to call without holding d.mu (deviceSet.set is
// internally locked; WorkspaceFor only reads state).
func (d *Daemon) seedDeviceFor(a agentstate.Agent) {
	if len(a.SignSeed) != ed25519.SeedSize {
		return
	}
	ws, ok := d.state.WorkspaceFor(a.ID)
	if !ok || !ws.E2E {
		return
	}
	d.devices.set(a.ID, ed25519.NewKeyFromSeed(a.SignSeed).Public().(ed25519.PublicKey))
}

// attach wires an agent into the live daemon: ensures it has a runtime, is in
// state, and—if the daemon is serving—has its inbox + presence loops running.
// Idempotent (a second call for the same agent is a no-op for loops). Caller
// must NOT hold d.mu.
func (d *Daemon) attach(a agentstate.Agent) {
	d.mu.Lock()
	if d.runtimes[a.ID] == nil {
		d.runtimes[a.ID] = newRuntime(a.ID, 1000)
	}
	d.state.Upsert(a)
	startLoops := d.serving && d.cancels[a.ID] == nil
	var loopCtx context.Context
	if startLoops {
		var cancel context.CancelFunc
		loopCtx, cancel = context.WithCancel(d.runCtx)
		d.cancels[a.ID] = cancel
	}
	rt := d.runtimes[a.ID]
	ctl := d.ctl
	d.mu.Unlock()

	// Seed the local device set for e2e agents (outside mu hold; both calls are
	// independently locked).
	d.seedDeviceFor(a)

	if !startLoops {
		return
	}
	// If a detach races in between the unlock and here, it cancels loopCtx; the
	// loops then start on an already-cancelled context and exit promptly — the
	// correct outcome for a create-then-immediately-remove.
	go runInbox(loopCtx, rt, d.hub, a.InboxChannel, a.Cursor, func(cur string) {
		_ = agentstate.SetCursor(d.statePath, a.ID, cur)
	}, d.openerFor(a.ID))
	go runPresence(loopCtx, ctl, a)
}

// detach stops an agent's loops and drops its runtime, cached handler, and
// state entry. Caller must NOT hold d.mu.
func (d *Daemon) detach(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cancel := d.cancels[id]; cancel != nil {
		cancel()
		delete(d.cancels, id)
	}
	for _, a := range d.state.Agents {
		if a.ID == id {
			delete(d.handlers, a.Key)
		}
	}
	delete(d.runtimes, id)
	d.state.Remove(id)
}

// agentByKeyLocked finds an agent by capability key in in-memory state.
// Caller holds d.mu.
func (d *Daemon) agentByKeyLocked(key string) (agentstate.Agent, bool) {
	for _, a := range d.state.Agents {
		if a.Key == key {
			return a, true
		}
	}
	return agentstate.Agent{}, false
}

// reloadFromDisk additively merges agents present on disk but not in memory
// (self-heals the cross-process case: another process onboarded an agent after
// our boot snapshot). Never drops in-memory agents. No-op if statePath is empty.
// Returns true if it added at least one agent.
func (d *Daemon) reloadFromDisk() bool {
	if d.statePath == "" {
		return false
	}
	st, err := agentstate.Load(d.statePath)
	if err != nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	added := false
	for _, a := range st.Agents {
		// Dedup by capability key (the routing identity) — that's what callers
		// look up by. attach later dedups loop-startup by agent ID.
		if _, known := d.agentByKeyLocked(a.Key); !known {
			d.state.Agents = append(d.state.Agents, a)
			added = true
		}
	}
	return added
}

// resolveHandler returns the MCP handler for a capability key, attaching the
// agent (and reloading from disk if needed) on first use. Returns nil if no
// agent owns the key.
func (d *Daemon) resolveHandler(key string) http.Handler {
	d.mu.Lock()
	h := d.handlers[key]
	a, ok := d.agentByKeyLocked(key)
	d.mu.Unlock()
	if h != nil {
		return h
	}
	if !ok {
		if d.reloadFromDisk() {
			d.mu.Lock()
			a, ok = d.agentByKeyLocked(key)
			d.mu.Unlock()
		}
	}
	if !ok {
		return nil
	}
	d.attach(a) // idempotent for loop start (handler may be built twice under a concurrent first-request race; the duplicate is discarded)
	d.mu.Lock()
	defer d.mu.Unlock()
	if h := d.handlers[key]; h != nil {
		return h
	}
	nh := newAgentHandler(&agentMCP{ops: d, agentID: a.ID, from: a.Name})
	d.handlers[key] = nh
	return nh
}

// mux serves every agent's MCP endpoint at /a/<key> through a single catch-all
// that resolves (and lazily attaches) the agent per request, so agents onboarded
// after startup are served without a daemon restart. The unguessable capability
// key in the path is the localhost auth and selects the agent's runtime.
func (d *Daemon) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/a/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/a/")
		if key == "" || strings.Contains(key, "/") {
			http.NotFound(w, r)
			return
		}
		h := d.resolveHandler(key)
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	return m
}

// Addr is the resolved MCP listen address.
func (d *Daemon) Addr() string {
	if d.state.Daemon.MCPAddr != "" {
		return d.state.Daemon.MCPAddr
	}
	return DefaultMCPAddr
}

// RunOn starts inbox/presence loops and serves the MCP mux on a pre-bound
// listener (so the caller can hold the port as a single-runtime mutex).
func (d *Daemon) RunOn(ctx context.Context, ln net.Listener) error {
	ctl := d.control
	if ctl == nil {
		ctl = control.NewClient(d.state.Daemon.RouterURL)
	}
	g, gctx := errgroup.WithContext(ctx)
	d.mu.Lock()
	d.runCtx = gctx
	d.ctl = ctl
	d.serving = true
	initial := append([]agentstate.Agent(nil), d.state.Agents...)
	d.mu.Unlock()
	for _, a := range initial {
		d.attach(a)
	}
	srv := &http.Server{Handler: d.mux()}
	g.Go(func() error {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	// Close on cancel; ignore Close's own error so it can't mask the cause that
	// cancelled gctx (which is the error g.Wait() should surface).
	g.Go(func() error { <-gctx.Done(); _ = srv.Close(); return nil })
	err := g.Wait()
	d.mu.Lock()
	d.serving = false
	d.mu.Unlock()
	return err
}

// Run binds Addr() itself, then delegates to RunOn (back-compat).
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", d.Addr())
	if err != nil {
		return err
	}
	return d.RunOn(ctx, ln)
}
