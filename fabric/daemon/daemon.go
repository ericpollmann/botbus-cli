package daemon

import (
	"context"
	"net/http"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/mark3labs/mcp-go/server"
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
		ag := &agentMCP{rt: d.runtimes[a.ID], hub: d.hub, outbound: d.state.Daemon.OutboundChannel, from: a.Name}
		s := buildMCPServer(ag)
		m.Handle(path, server.NewStreamableHTTPServer(s, server.WithEndpointPath(path)))
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

// Run starts every agent's inbox + presence loops and serves the MCP mux until
// ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
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

	srv := &http.Server{Addr: d.Addr(), Handler: d.mux()}
	g.Go(func() error {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	g.Go(func() error { <-gctx.Done(); return srv.Close() })

	return g.Wait()
}
