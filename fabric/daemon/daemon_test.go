package daemon

import (
	"crypto/ed25519"
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

// TestSeedLocalTrustRegistersE2EAgent verifies that seedLocalTrust populates the
// local trust graph for agents with a valid SignSeed in an e2e workspace:
//   - a root agent (no Parent) is admitted as an anchor
//   - a child agent gets a parent-signed cert and is resolvable via trust.resolve
//   - a plain (non-e2e) agent is a no-op
func TestSeedLocalTrustRegistersE2EAgent(t *testing.T) {
	_, rootPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	_, childPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	rootSeed := rootPriv.Seed()
	childSeed := childPriv.Seed()
	childPub := childPriv.Public().(ed25519.PublicKey)

	rootID := "root-1"
	childID := "child-1"
	st := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: rootID, Name: "ws-root", Key: "k0", InboxChannel: "i0", SignSeed: rootSeed},
			{ID: childID, Name: "e2e-member", Key: "k1", InboxChannel: "i1",
				Parent: rootID, SignSeed: childSeed},
		},
		Workspaces: []agentstate.Workspace{
			{RootID: rootID, E2E: true, Epoch: 1},
		},
	}
	d := NewRuntime(Config{State: st, Hub: hubclient.NewFake()})

	// Seed root — it is the workspace root so it becomes an anchor.
	rootAgent, _ := st.Get(rootID)
	d.seedLocalTrust(rootAgent)

	if _, ok := d.trust.anchors.lookup(rootID); !ok {
		t.Fatal("root agent must be admitted as anchor after seedLocalTrust")
	}

	// Seed child — it has a parent with a SignSeed; a cert should be added and the
	// child must be resolvable via trust.resolve.
	childAgent, _ := st.Get(childID)
	d.seedLocalTrust(childAgent)

	got, ok := d.trust.resolve(childID)
	if !ok {
		t.Fatal("child agent must be resolvable via trust graph after seedLocalTrust")
	}
	if !got.Equal(childPub) {
		t.Fatalf("resolved pubkey mismatch: got %x, want %x", got, childPub)
	}

	// Non-e2e agent (no SignSeed) must not be registered.
	plainID := "plain-1"
	st.Agents = append(st.Agents, agentstate.Agent{
		ID: plainID, Name: "plain", Key: "k2", InboxChannel: "i2", Parent: rootID,
	})
	plain, _ := st.Get(plainID)
	d.seedLocalTrust(plain)
	if _, ok := d.trust.anchors.lookup(plainID); ok {
		t.Fatal("non-e2e agent should not appear as anchor")
	}
	if _, ok := d.trust.resolve(plainID); ok {
		t.Fatal("non-e2e agent should not be resolvable")
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
