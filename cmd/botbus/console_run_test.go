package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/ericpollmann/botbus-proto/wire"
)

// stubControl mints a fresh id per call and accepts any Bearer-authenticated
// register. Mirrors hostagent's test stub; the real auth/HMAC validation is
// exercised in the router's own tests.
func stubControl(t *testing.T) *control.Client {
	t.Helper()
	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mint", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("minted-%d", n.Add(1))})
	})
	mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return control.NewClient(srv.URL)
}

// fakeOps builds a *daemon.Daemon wired to a fake hub + stub control for
// integration-level onboardChildOps tests.
func fakeOps(t *testing.T, hub *hubclient.Fake, statePath string) *daemon.Daemon {
	t.Helper()
	st, _ := agentstate.Load(statePath)
	return daemon.NewRuntime(daemon.Config{
		State:     st,
		StatePath: statePath,
		Hub:       hub,
		Control:   stubControl(t),
		MintKey:   func() string { return "key-fixed" },
		Domain:    domain,
	})
}

func TestOnboardChildCreatesUnderRootAndSeedsWelcome(t *testing.T) {
	hub := hubclient.NewFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	p := &profile.Profile{
		User:    "eric",
		Framing: "works async",
		Root:    profile.Root{ID: "root-id", InboxChannel: "root-inbox", Key: "root-key"},
	}

	// Build ops with the profile (so root() resolves via profile).
	st, _ := agentstate.Load(statePath)
	opsWithProfile := daemon.NewRuntime(daemon.Config{
		State:     st,
		StatePath: statePath,
		Hub:       hub,
		Control:   stubControl(t),
		Profile:   p,
		MintKey:   func() string { return "key-fixed" },
		Domain:    domain,
	})

	inst, err := onboardChildOps(context.Background(), opsWithProfile, "myth-compiler", "packages/compile")
	if err != nil {
		t.Fatalf("onboardChildOps: %v", err)
	}

	// The child must be persisted with Parent == the root id.
	agents, err := hostagent.List(statePath)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("want 1 child agent, got %d", len(agents))
	}
	child := agents[0]
	if child.Name != "myth-compiler" {
		t.Fatalf("child name = %q", child.Name)
	}
	if child.Parent != "root-id" {
		t.Fatalf("child parent = %q, want root-id", child.Parent)
	}
	if child.Focus != "packages/compile" {
		t.Fatalf("child focus = %q", child.Focus)
	}

	// A welcome must have been published into the child's inbox.
	published := hub.Published(child.InboxChannel)
	if len(published) != 1 {
		t.Fatalf("want 1 welcome published to inbox, got %d", len(published))
	}
	if !strings.Contains(published[0], "myth-compiler") || !strings.Contains(published[0], "packages/compile") {
		t.Fatalf("welcome missing agent/focus: %q", published[0])
	}
	if !strings.Contains(published[0], "eric") {
		t.Fatalf("welcome should embed operator framing/name: %q", published[0])
	}

	// The returned instructions must carry the MCP command and channel URL.
	if !strings.Contains(inst.MCPCommand, "claude mcp add") {
		t.Fatalf("onboardChildOps MCPCommand should contain MCP command, got %q", inst.MCPCommand)
	}
	if !strings.Contains(inst.ChannelURL, child.InboxChannel) {
		t.Fatalf("onboardChildOps ChannelURL should contain inbox channel, got %q", inst.ChannelURL)
	}
}

func TestOnboardChildPropagatesCreateError(t *testing.T) {
	hub := hubclient.NewFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	ops := fakeOps(t, hub, statePath)
	// No root set → CreateChild will fail trying to find root.
	if _, err := onboardChildOps(context.Background(), ops, "", "focus"); err == nil {
		t.Fatal("expected error onboarding an unnamed child")
	}
}

// The `o` key in roster mode drives a two-step name → focus prompt that calls
// the injected onboard action and surfaces the connect URL.
func TestOnboardInlineFlow(t *testing.T) {
	var gotName, gotFocus string
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i-root"}})
	m.onboard = func(name, focus string) (daemon.ConnectInstructions, error) {
		gotName, gotFocus = name, focus
		return daemon.ConnectInstructions{ChannelURL: "https://child-inbox.botbus.ai"}, nil
	}

	// Press `o` to begin onboarding.
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if m.onboardState != onboardAskName {
		t.Fatalf("o should start onboard at name step, got state %d", m.onboardState)
	}

	// Type a name and Enter → advance to focus step.
	m = typeRunes(t, m, "myth-cli")
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.onboardState != onboardAskFocus {
		t.Fatalf("after name enter, want focus step, got %d", m.onboardState)
	}

	// Type a focus and Enter → mint + show connect URL.
	m = typeRunes(t, m, "cli stuff")
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.onboardState != onboardOff {
		t.Fatalf("after focus enter, onboarding should be done, got %d", m.onboardState)
	}
	if gotName != "myth-cli" || gotFocus != "cli stuff" {
		t.Fatalf("onboard called with (%q,%q)", gotName, gotFocus)
	}
	if !strings.Contains(m.onboardMsg, "child-inbox.botbus.ai") {
		t.Fatalf("result message should carry connect URL, got %q", m.onboardMsg)
	}
	if !strings.Contains(m.View(), "child-inbox.botbus.ai") {
		t.Fatal("roster view should render the connect URL result")
	}
}

// An empty name at the onboard name step shows an error and stays on the step.
func TestOnboardEmptyNameStays(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	m.onboard = func(string, string) (daemon.ConnectInstructions, error) {
		return daemon.ConnectInstructions{}, nil
	}
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter}) // enter with empty name
	if m.onboardState != onboardAskName {
		t.Fatalf("empty name should stay on name step, got %d", m.onboardState)
	}
	if m.onboardMsg == "" {
		t.Fatal("empty name should set an error message")
	}
}

// A failing onboard action surfaces the error and returns to the plain roster.
func TestOnboardActionErrorSurfaces(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	m.onboard = func(string, string) (daemon.ConnectInstructions, error) {
		return daemon.ConnectInstructions{}, fmt.Errorf("boom")
	}
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m = typeRunes(t, m, "child")
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter}) // name → focus
	m = typeRunes(t, m, "focus")
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter}) // focus → mint (errors)
	if m.onboardState != onboardOff {
		t.Fatalf("failed onboard should return to roster, got state %d", m.onboardState)
	}
	if !strings.Contains(m.onboardMsg, "boom") {
		t.Fatalf("error message should surface, got %q", m.onboardMsg)
	}
}

// Non-key messages during onboarding (e.g. window resize) are forwarded to the
// input without advancing the onboard step.
func TestOnboardForwardsNonKeyMessages(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	m.onboard = func(string, string) (daemon.ConnectInstructions, error) {
		return daemon.ConnectInstructions{}, nil
	}
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m, _ = updConsole(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.onboardState != onboardAskName {
		t.Fatalf("window resize should not change onboard step, got %d", m.onboardState)
	}
}

// `o` is inert when no onboard action is wired (the direct-chat path).
func TestOnboardKeyInertWithoutHook(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if m.onboardState != onboardOff {
		t.Fatal("o with no onboard hook should be inert")
	}
}

// esc during the onboard prompt aborts back to the plain roster (does NOT quit).
func TestOnboardEscAborts(t *testing.T) {
	m := newConsoleModel([]wire.AgentNode{{Name: "root", InboxChannel: "i"}})
	m.onboard = func(string, string) (daemon.ConnectInstructions, error) {
		return daemon.ConnectInstructions{}, nil
	}
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m, cmd := updConsole(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.onboardState != onboardOff {
		t.Fatal("esc should abort onboarding")
	}
	if cmd != nil {
		t.Fatal("esc during onboarding should not quit")
	}
}

// A real (non-zero) chatSession returned by startChat must be bound onto the
// model — the model rebinds its transport channels + display name and returns a
// non-nil Cmd batch so it starts consuming the fresh channels on dip-in.
func TestDipInBindsSessionAndPumpsChannels(t *testing.T) {
	recv := make(chan []byte, 1)
	states := make(chan connState, 1)
	send := make(chan []byte, 1)
	seed := make(chan seedMsg, 1)

	m := newConsoleModel([]wire.AgentNode{{Name: "myth-compiler", InboxChannel: "i-c1"}})
	m.startChat = func(channel string) chatSession {
		if channel != "i-c1" {
			t.Fatalf("startChat channel = %q, want i-c1", channel)
		}
		return chatSession{
			recv: recv, states: states, send: send, seed: seed,
			name: "operator", host: "i-c1.botbus.ai", histBase: "https://i-c1.botbus.ai",
		}
	}
	m.stopChat = func() {}

	m, cmd := updConsole(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeChat {
		t.Fatalf("dip should switch to chat mode, got %d", m.mode)
	}
	if cmd == nil {
		t.Fatal("dip with a real session should return a Cmd batch to pump the channels")
	}
	if m.myName != "operator" {
		t.Fatalf("display name should rebind to session name, got %q", m.myName)
	}
	if m.host != "i-c1.botbus.ai" || m.histBase != "https://i-c1.botbus.ai" {
		t.Fatalf("host/histBase not rebound: host=%q histBase=%q", m.host, m.histBase)
	}

	// The model must now consume the rebound recv channel: feed it a frame and
	// run the model's recv-wait Cmd; it should surface as an incoming message.
	recv <- []byte("peer: hi there")
	got := waitRecv(m.recv, m.epoch)()
	if _, ok := got.(incoming); !ok {
		t.Fatalf("rebound recv not consumed by model; waitRecv yielded %T", got)
	}
}

// A message from a torn-down dip (stale epoch) must be dropped: the
// close-driven "stream closed" errMsg from session N must not pollute the
// scrollback after the user has dipped out and back into session N+1. A
// current-epoch incoming must still append normally.
func TestStaleEpochMessagesAreDropped(t *testing.T) {
	recv := make(chan []byte, 1)
	states := make(chan connState, 1)
	send := make(chan []byte, 1)
	seed := make(chan seedMsg, 1)

	m := newConsoleModel([]wire.AgentNode{{Name: "a", InboxChannel: "i-a"}})
	m.startChat = func(string) chatSession {
		return chatSession{recv: recv, states: states, send: send, seed: seed, name: "op", host: "h", histBase: "b"}
	}
	m.stopChat = func() {}

	// First dip-in: epoch becomes 1.
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.epoch != 1 {
		t.Fatalf("first dip-in should bump epoch to 1, got %d", m.epoch)
	}
	staleEpoch := m.epoch

	// Dip out (esc → roster), then dip back in: epoch becomes 2.
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeRoster {
		t.Fatalf("esc should return to roster, got mode %d", m.mode)
	}
	m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.epoch != 2 {
		t.Fatalf("second dip-in should bump epoch to 2, got %d", m.epoch)
	}

	// A stale "stream closed" errMsg from the FIRST session arrives late.
	before := len(m.msgs)
	m, _ = updConsole(m, errMsg{fmt.Errorf("stream closed"), staleEpoch})
	if len(m.msgs) != before {
		t.Fatalf("stale errMsg must not be appended; msgs grew from %d to %d", before, len(m.msgs))
	}
	if m.mode != modeChat {
		t.Fatalf("stale message must not change mode; got %d", m.mode)
	}

	// A stale incoming is likewise dropped.
	m, _ = updConsole(m, incoming{data: []byte("ghost: boo"), epoch: staleEpoch})
	if len(m.msgs) != before {
		t.Fatalf("stale incoming must not be appended; msgs = %d", len(m.msgs))
	}

	// A current-epoch incoming IS appended.
	m, _ = updConsole(m, incoming{data: []byte("peer: live"), epoch: m.epoch})
	if len(m.msgs) != before+1 {
		t.Fatalf("current-epoch incoming should append; msgs = %d, want %d", len(m.msgs), before+1)
	}
	if !strings.Contains(m.msgs[len(m.msgs)-1], "live") {
		t.Fatalf("appended message should be the live one, got %q", m.msgs[len(m.msgs)-1])
	}
}

// wireConsoleChat installs working start/stop/onboard hooks; stopChat before any
// dip must be a no-op (no panic on nil cancel).
func TestWireConsoleChatHooksInstalled(t *testing.T) {
	m := newConsoleModel(nil)
	hub := hubclient.NewFake()
	statePath := filepath.Join(t.TempDir(), "state.json")
	ops := fakeOps(t, hub, statePath)
	wireConsoleChat(context.Background(), &m, ops)
	if m.startChat == nil || m.stopChat == nil || m.onboard == nil {
		t.Fatal("wireConsoleChat should install all three hooks")
	}
	m.stopChat() // no active dip → must not panic
}

// typeRunes feeds each rune of s into the model as a key message.
func typeRunes(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		m, _ = updConsole(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}
