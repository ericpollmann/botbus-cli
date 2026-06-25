package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
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

// TestMuxServesKnownKeyAnd404sUnknown verifies the catch-all mux dispatches to
// a known key and returns 404 for an unknown one.
func TestMuxServesKnownKeyAnd404sUnknown(t *testing.T) {
	st := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out"},
		Agents: []agentstate.Agent{{ID: "myth-compiler", Key: "key-xyz", InboxChannel: "inbox-c"}},
	}
	d := New(st, "", hubclient.NewFake())
	m := d.mux()

	// Known key → routed to the MCP handler (non-404). The body "{}" is valid
	// JSON but not a valid MCP JSON-RPC request, so the handler replies 400 —
	// which still proves the route resolved (the assertion is "not 404").
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/a/key-xyz", strings.NewReader("{}")))
	if rr.Code == http.StatusNotFound {
		t.Fatalf("known key returned 404, want non-404, got %d", rr.Code)
	}

	// Unknown key → 404.
	rr2 := httptest.NewRecorder()
	m.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/a/unknownkey", strings.NewReader("{}")))
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("unknown key should return 404, got %d", rr2.Code)
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

func TestNewRuntimeWiresFields(t *testing.T) {
	st := &agentstate.State{Daemon: agentstate.Daemon{MCPAddr: "127.0.0.1:0"}}
	d := NewRuntime(Config{
		State: st, StatePath: "/tmp/x.json", Hub: hubclient.NewFake(),
		Control: control.NewClient("http://r"), MintKey: func() string { return "k" },
		Domain: "botbus.ai",
	})
	if d.domain != "botbus.ai" || d.mintKey == nil || d.control == nil {
		t.Fatalf("NewRuntime did not wire fields: %+v", d)
	}
	if d.Addr() != "127.0.0.1:0" {
		t.Fatalf("Addr=%q", d.Addr())
	}
	// Back-compat: New still constructs a usable Daemon.
	if New(st, "/tmp/x.json", hubclient.NewFake()) == nil {
		t.Fatal("New returned nil")
	}
}
