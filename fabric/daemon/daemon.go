package daemon

import (
	"context"
	"net"
	"net/http"

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
	state     *agentstate.State
	statePath string
	hub       hubclient.HubClient
	runtimes  map[string]*AgentRuntime

	control *control.Client
	profile *profile.Profile
	mintKey func() string
	domain  string
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
	}
}

// New is the back-compat constructor (inbox/MCP only; control built lazily in Run).
func New(state *agentstate.State, statePath string, hub hubclient.HubClient) *Daemon {
	return NewRuntime(Config{State: state, StatePath: statePath, Hub: hub})
}

// mux mounts one MCP endpoint per agent at /a/<key>. The unguessable capability
// key in the path is the localhost auth and selects the agent's runtime.
func (d *Daemon) mux() *http.ServeMux {
	m := http.NewServeMux()
	for _, a := range d.state.Agents {
		path := "/a/" + a.Key
		ag := &agentMCP{ops: d, agentID: a.ID, from: a.Name}
		m.Handle(path, newAgentHandler(ag, path))
	}
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
	for _, a := range d.state.Agents {
		a := a
		rt := d.runtimes[a.ID]
		g.Go(func() error {
			runInbox(gctx, rt, d.hub, a.InboxChannel, a.Cursor, func(cur string) {
				_ = agentstate.SetCursor(d.statePath, a.ID, cur)
			})
			return nil
		})
		g.Go(func() error { runPresence(gctx, ctl, a); return nil })
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
	return g.Wait()
}

// Run binds Addr() itself, then delegates to RunOn (back-compat).
func (d *Daemon) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", d.Addr())
	if err != nil {
		return err
	}
	return d.RunOn(ctx, ln)
}
