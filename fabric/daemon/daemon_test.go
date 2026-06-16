package daemon

import (
	"net/http/httptest"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestNewBuildsRuntimePerAgent(t *testing.T) {
	st := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "a", Key: "k-a", InboxChannel: "i-a"},
		{ID: "b", Key: "k-b", InboxChannel: "i-b"},
	}}
	d := New(st, "", hubclient.NewFake())
	if len(d.runtimes) != 2 || d.runtimes["a"] == nil || d.runtimes["b"] == nil {
		t.Fatalf("runtimes = %v", d.runtimes)
	}
}

func TestMuxMountsPerAgentEndpointAndRejectsUnknownKey(t *testing.T) {
	st := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out"},
		Agents: []agentstate.Agent{{ID: "myth-compiler", Key: "key-xyz", InboxChannel: "inbox-c"}},
	}
	d := New(st, "", hubclient.NewFake())
	mux := d.mux()

	// The agent's MCP endpoint is routed at /a/<key>.
	if _, pat := mux.Handler(httptest.NewRequest("GET", "/a/key-xyz", nil)); pat == "" {
		t.Fatal("expected /a/key-xyz to be routed")
	}
	// An unknown key is not routed (ServeMux 404, empty pattern).
	if _, pat := mux.Handler(httptest.NewRequest("GET", "/a/wrong", nil)); pat != "" {
		t.Fatalf("unknown key should not match, got pattern %q", pat)
	}
}

func TestAddrDefaults(t *testing.T) {
	d := New(&agentstate.State{}, "", hubclient.NewFake())
	if d.Addr() != DefaultMCPAddr {
		t.Fatalf("Addr = %q, want %q", d.Addr(), DefaultMCPAddr)
	}
	d2 := New(&agentstate.State{Daemon: agentstate.Daemon{MCPAddr: "127.0.0.1:9999"}}, "", hubclient.NewFake())
	if d2.Addr() != "127.0.0.1:9999" {
		t.Fatalf("Addr = %q, want override", d2.Addr())
	}
}
