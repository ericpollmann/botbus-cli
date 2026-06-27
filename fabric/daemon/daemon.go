package daemon

import (
	"context"
	"crypto/ed25519"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/e2e"
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
	trust    *trustGraph
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
		trust:    newTrustGraph(),
		replay:   newReplayWindow(),
	}
}

// New is the back-compat constructor (inbox/MCP only; control built lazily in Run).
func New(state *agentstate.State, statePath string, hub hubclient.HubClient) *Daemon {
	return NewRuntime(Config{State: state, StatePath: statePath, Hub: hub})
}

// seedLocalTrust registers the agent's ed25519 pubkey in the local trust graph
// when the agent has a valid SignSeed and belongs to an e2e workspace. Root
// agents (no parent) are added as admitted anchors; child agents get a
// parent-signed cert added to the graph so they resolve up the chain.
// Safe to call without holding d.mu (trust graph and WorkspaceFor are
// independently locked; state reads are read-only here).
func (d *Daemon) seedLocalTrust(a agentstate.Agent) {
	if len(a.SignSeed) != ed25519.SeedSize {
		return
	}
	ws, ok := d.state.WorkspaceFor(a.ID)
	if !ok || !ws.E2E {
		return
	}
	childPub := ed25519.NewKeyFromSeed(a.SignSeed).Public().(ed25519.PublicKey)
	if a.Parent == "" || d.state.WorkspaceRootID(a.ID) == a.ID {
		// Root agent — admit as an anchor.
		d.trust.anchors.set(a.ID, childPub)
		return
	}
	// Child agent — add a parent-signed cert so it resolves via the anchor chain.
	parent, ok := d.state.AgentByID(a.Parent)
	if !ok || len(parent.SignSeed) != ed25519.SeedSize {
		return
	}
	parentPriv := ed25519.NewKeyFromSeed(parent.SignSeed)
	d.trust.addCert(e2e.SignCert(parentPriv, a.ID, a.Parent, childPub))
}

// hydrateWorkspaceTrust rebuilds the in-memory trust graph for ws from persisted
// state so a fresh process (one-shot CLI or restarted daemon) has the COMPLETE
// anchor set: local agents (root → anchor, children → parent-signed certs) plus
// every persisted remote anchor. Idempotent.
func (d *Daemon) hydrateWorkspaceTrust(ws *agentstate.Workspace) {
	for _, a := range d.state.Agents {
		if d.state.WorkspaceRootID(a.ID) == ws.RootID {
			d.seedLocalTrust(a)
		}
	}
	for _, ar := range ws.Anchors {
		if len(ar.SignPub) == ed25519.SeedSize || len(ar.SignPub) == ed25519.PublicKeySize {
			d.trust.anchors.set(ar.ID, ed25519.PublicKey(ar.SignPub))
		}
	}
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

	// Seed the local trust graph for e2e agents (outside mu hold; both calls are
	// independently locked).
	d.seedLocalTrust(a)

	// Publish this agent's cert to the roster channel so remote hosts learn it.
	// Best-effort: only when hub + roster are configured and agent is a non-root
	// e2e child with a parent signing seed.
	if d.hub != nil && len(a.SignSeed) == ed25519.SeedSize && a.Parent != "" {
		if ws, ok := d.state.WorkspaceFor(a.ID); ok && ws.E2E && ws.Roster != "" {
			if parent, ok := d.state.AgentByID(a.Parent); ok && len(parent.SignSeed) == ed25519.SeedSize {
				childPub := ed25519.NewKeyFromSeed(a.SignSeed).Public().(ed25519.PublicKey)
				parentPriv := ed25519.NewKeyFromSeed(parent.SignSeed)
				cert := e2e.SignCert(parentPriv, a.ID, a.Parent, childPub)
				if err := d.publishCert(context.Background(), ws, cert); err != nil {
					log.Printf("daemon: publishCert for %s: %v", a.ID, err)
				}
			}
		}
	}

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

// persistWorkspaceKey best-effort saves ws's key/epoch to state.json after a
// roster-ingested rotation. No-op when statePath is unset (tests).
func (d *Daemon) persistWorkspaceKey(ws *agentstate.Workspace) {
	if d.statePath == "" {
		return
	}
	if err := agentstate.Save(d.statePath, d.state); err != nil {
		log.Printf("roster: persist workspace key: %v", err)
	}
}

// Run binds Addr() itself, then delegates to RunOn (back-compat).
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", d.Addr())
	if err != nil {
		return err
	}
	return d.RunOn(ctx, ln)
}
