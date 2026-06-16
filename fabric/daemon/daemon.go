package daemon

import (
	"context"
	"net/http"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
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
}

// New builds a Daemon from loaded state. statePath is where cursor advances are
// persisted; hub is the (real or fake) hub client.
func New(state *agentstate.State, statePath string, hub hubclient.HubClient) *Daemon {
	rts := make(map[string]*AgentRuntime, len(state.Agents))
	for _, a := range state.Agents {
		rts[a.ID] = newRuntime(a.ID, 1000)
	}
	return &Daemon{state: state, statePath: statePath, hub: hub, runtimes: rts}
}

// mux mounts one MCP endpoint per agent at /a/<key>. The unguessable capability
// key in the path is the localhost auth and selects the agent's runtime.
func (d *Daemon) mux() *http.ServeMux {
	m := http.NewServeMux()
	for _, a := range d.state.Agents {
		path := "/a/" + a.Key
		ag := &agentMCP{rt: d.runtimes[a.ID], hub: d.hub, outbound: d.state.Daemon.OutboundChannel, from: a.ID}
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
	ctl := control.NewClient(d.state.Daemon.RouterURL)
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
