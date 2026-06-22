# Unify TUI + MCP over one local-agent core — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce one `Ops` facade owned by the daemon runtime, and route every caller (MCP tools + the TUI) through it, so `add`/`list`/`dm`/`send`/`read` have a single implementation and can't diverge.

**Architecture:** The existing `*daemon.Daemon` is promoted to the single local-agent runtime. It gains the collaborators it currently builds ad-hoc (control client, operator profile, key minter, domain) and exposes a small `Ops` interface that wraps the already-factored primitives (`hostagent.*`, `daemon.Send`/`Next`, `control.Roster`, `console.RenderWelcome`/`SeedWelcome`). The MCP tool handlers and the console call `Ops` instead of those primitives directly. `botbus` (no-args) runs one runtime with both the TUI and the MCP mux; `botbus daemon` runs it headless. One runtime per host, enforced by the MCP port bind.

**Tech Stack:** Go 1.25, `github.com/mark3labs/mcp-go`, `github.com/ericpollmann/botbus-proto` (`wire`, `envelope`, `hubclient`, `keys`), `golang.org/x/sync/errgroup`, bubbletea. Tests use `miniredis`-backed control stubs and `hubclient.NewFake()`.

## Global Constraints

- Module: `github.com/ericpollmann/botbus-cli`; Go `1.25`.
- No new third-party deps — reuse what the repo already imports.
- Tests: `go build ./... && go vet ./... && go test -race ./...` must be green before every commit. Target 100% coverage on new code (`fabric/daemon/ops*.go`).
- The runtime is a **singleton per host**: only one process may own the inbox subscriptions + MCP port at a time.
- `ConnectInstructions` is **MCP-first** (daemon-hosted endpoint), with the raw channel URL as fallback.
- Default MCP addr: `daemon.DefaultMCPAddr = "127.0.0.1:8765"`. Domain: `"botbus.ai"`.
- Deferred (do NOT build): live child-process spawn, hub-level dm privacy, HTTP/React face, TUI auto-attach, hub-recipe MCP mention. (Tracked in the spec's Follow-ups.)

## File structure

- **Create** `fabric/daemon/ops.go` — `Ops` interface + `ConnectInstructions` struct.
- **Create** `fabric/daemon/ops_impl.go` — the `*Daemon` methods implementing `Ops` (+ small helpers `root()`, `hostDeps()`).
- **Create** `fabric/daemon/ops_impl_test.go` — unit tests for every op.
- **Modify** `fabric/daemon/daemon.go` — add `control`, `profile`, `mintKey`, `domain` fields; add `Config` + `NewRuntime(Config)`; keep `New(...)` as a thin delegator; `Run` uses `d.control` if set.
- **Modify** `fabric/daemon/mcp.go` — `agentMCP` carries an `Ops` + `agentID`; `toolSend`/`toolNext` call `Ops`.
- **Modify** `cmd/botbus/console.go` — `firstRun`/`onboardChild`/`runConsole`/`wireConsoleChat` call `Ops`; show MCP-first connect instructions; build one runtime.
- **Modify** `cmd/botbus/daemon.go` — `daemonCmd` builds the runtime via `NewRuntime`.
- **Modify** `cmd/botbus/main.go` (or `console.go`) — single-runtime fail-fast on port bind.

Note (spec refinement): the spec listed TUI "dip-in = ReadInbox + Send". In the code, dip-in is a **channel WebSocket subscription** (`wireConsoleChat`/`runWSText`), which is orthogonal to the per-agent inbox queue `ReadInbox` reads. Dip-in stays as-is; the unified ops are `Roster`/`CreateChild`/`Send`/`ReadInbox`/`EnsureRoot`. The TUI gains a *real* dm through `Ops.Send(from=root, to=[name], kind="dm")` (vs. today's display-only `/dm`).

---

### Task 1: `Ops` interface, `ConnectInstructions`, and the runtime `Config`

**Files:**
- Create: `fabric/daemon/ops.go`
- Modify: `fabric/daemon/daemon.go`
- Test: `fabric/daemon/daemon_test.go` (add cases; create if absent)

**Interfaces:**
- Produces: `daemon.Ops` interface; `daemon.ConnectInstructions{MCPCommand, MCPEndpoint, ChannelURL string}`; `daemon.Config{State, StatePath, Hub, Control, Profile, MintKey, Domain}`; `daemon.NewRuntime(Config) *Daemon`. `*Daemon` gains fields `control *control.Client`, `profile *profile.Profile`, `mintKey func() string`, `domain string`.

- [ ] **Step 1: Write the failing test** — append to `fabric/daemon/daemon_test.go`:

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestNewRuntimeWiresFields`
Expected: FAIL — `NewRuntime`, `Config`, `d.domain` undefined.

- [ ] **Step 3: Create `fabric/daemon/ops.go`**

```go
package daemon

import (
	"context"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/wire"
)

// ConnectInstructions tells the operator how to attach an agent to its inbox.
// MCPCommand/MCPEndpoint are preferred when the runtime hosts a local MCP
// endpoint; ChannelURL is the raw curl-recipe fallback.
type ConnectInstructions struct {
	MCPCommand  string // `claude mcp add --transport http <name> http://<addr>/a/<key>`
	MCPEndpoint string // http://<addr>/a/<key>
	ChannelURL  string // https://<inbox>.<domain>/
}

// Ops is the single local-agent operation surface every face (TUI, MCP, and a
// future HTTP face) calls. Implemented by *Daemon so there is exactly one
// implementation of each operation.
type Ops interface {
	Roster(ctx context.Context) ([]wire.AgentNode, error)
	CreateChild(ctx context.Context, name, focus string) (agentstate.Agent, ConnectInstructions, error)
	Send(ctx context.Context, fromAgent, body string, to []string, kind string) error
	ReadInbox(ctx context.Context, agentID string, timeoutSec int) (string, error)
	EnsureRoot(ctx context.Context) (agentstate.Agent, error)
}
```

- [ ] **Step 4: Extend `fabric/daemon/daemon.go`** — add fields + `Config`/`NewRuntime`, keep `New` as delegator, make `Run` reuse `d.control`:

Add the imports `"github.com/ericpollmann/botbus-cli/fabric/profile"` to the file. Replace the struct + `New` with:

```go
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
```

In `Run`, replace `ctl := control.NewClient(d.state.Daemon.RouterURL)` with:

```go
	ctl := d.control
	if ctl == nil {
		ctl = control.NewClient(d.state.Daemon.RouterURL)
	}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestNewRuntimeWiresFields`
Expected: PASS. Then `go build ./...` (catch any caller of `New` — should be unaffected).

- [ ] **Step 6: Commit**

```bash
cd /tmp/botbus-impl
git add fabric/daemon/ops.go fabric/daemon/daemon.go fabric/daemon/daemon_test.go
git commit -m "feat(daemon): Ops interface + ConnectInstructions + NewRuntime config"
```

---

### Task 2: `Roster` op + `root()` helper

**Files:**
- Create: `fabric/daemon/ops_impl.go`
- Test: `fabric/daemon/ops_impl_test.go`

**Interfaces:**
- Consumes: `control.Client.Roster(ctx, id, key)`, `hostagent.GetByName(statePath, "root")`, `d.profile`.
- Produces: `(*Daemon).Roster`; helper `(*Daemon).root() (agentstate.Agent, error)`.

- [ ] **Step 1: Write the failing test** — `fabric/daemon/ops_impl_test.go`:

```go
package daemon

import (
	"context"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestRosterUsesRootCreds(t *testing.T) {
	// stubRoster returns a single node iff the request carries root's id+key.
	srv := stubRoster(t, "root-id", "root-key")
	defer srv.Close()
	st := &agentstate.State{Agents: []agentstate.Agent{
		{ID: "root-id", Key: "root-key", Name: "root"},
	}}
	d := NewRuntime(Config{State: st, Hub: hubclient.NewFake(), Control: control.NewClient(srv.URL)})
	nodes, err := d.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "root" {
		t.Fatalf("nodes=%+v", nodes)
	}
}
```

Also add the test helper `stubRoster` at the bottom of the file (mirrors the existing `stubControl` style in `fabric/control/server_test.go`):

```go
import "net/http"
import "net/http/httptest"
import "encoding/json"
import "github.com/ericpollmann/botbus-proto/wire"

// stubRoster serves GET /v1/agents, returning one "root" node only when the
// request carries the expected X-Agent-Id + Bearer key.
func stubRoster(t *testing.T, wantID, wantKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Id") != wantID || r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]wire.AgentNode{{ID: wantID, Name: "root"}})
	}))
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestRosterUsesRootCreds`
Expected: FAIL — `d.Roster` undefined.

- [ ] **Step 3: Implement in `fabric/daemon/ops_impl.go`**

```go
package daemon

import (
	"context"
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/wire"
)

// root returns the operator's root agent (id + capability key), preferring the
// loaded profile and falling back to the local state entry named "root".
func (d *Daemon) root() (agentstate.Agent, error) {
	if d.profile != nil && d.profile.Root.ID != "" {
		return agentstate.Agent{
			ID: d.profile.Root.ID, Key: d.profile.Root.Key,
			Name: "root", InboxChannel: d.profile.Root.InboxChannel,
		}, nil
	}
	a, ok, err := hostagent.GetByName(d.statePath, "root")
	if err != nil {
		return agentstate.Agent{}, err
	}
	if !ok {
		return agentstate.Agent{}, fmt.Errorf("no root agent — run first-run setup")
	}
	return a, nil
}

// Roster returns the agent tree (parent links + liveness) as the root.
func (d *Daemon) Roster(ctx context.Context) ([]wire.AgentNode, error) {
	r, err := d.root()
	if err != nil {
		return nil, err
	}
	return d.control.Roster(ctx, r.ID, r.Key)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestRosterUsesRootCreds -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /tmp/botbus-impl
git add fabric/daemon/ops_impl.go fabric/daemon/ops_impl_test.go
git commit -m "feat(daemon): Ops.Roster + root() helper"
```

---

### Task 3: `EnsureRoot` op + `hostDeps()` helper

**Files:**
- Modify: `fabric/daemon/ops_impl.go`
- Test: `fabric/daemon/ops_impl_test.go`

**Interfaces:**
- Consumes: `hostagent.EnsureRoot(ctx, Deps)`, `d.hub`, `d.control`, `d.statePath`, `d.mintKey`.
- Produces: `(*Daemon).EnsureRoot`; helper `(*Daemon).hostDeps() hostagent.Deps`.

- [ ] **Step 1: Write the failing test** — append to `ops_impl_test.go`:

```go
func TestEnsureRootCreatesThenReuses(t *testing.T) {
	dir := t.TempDir()
	statePath := dir + "/state.json"
	srv := stubAcceptAll(t) // mint + register always 200
	defer srv.Close()
	d := NewRuntime(Config{
		State: &agentstate.State{}, StatePath: statePath,
		Hub: hubclient.NewFake(), Control: control.NewClient(srv.URL),
		MintKey: func() string { return "rootkey" }, Domain: "botbus.ai",
	})
	a1, err := d.EnsureRoot(context.Background())
	if err != nil || a1.Name != "root" {
		t.Fatalf("EnsureRoot #1: %v %+v", err, a1)
	}
	a2, err := d.EnsureRoot(context.Background())
	if err != nil || a2.ID != a1.ID {
		t.Fatalf("EnsureRoot #2 should reuse: %v %+v", err, a2)
	}
}
```

Add helper `stubAcceptAll` (mint returns an id; register/heartbeat 200):

```go
func stubAcceptAll(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/mint" {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "minted-id"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestEnsureRootCreatesThenReuses`
Expected: FAIL — `d.EnsureRoot` undefined.

- [ ] **Step 3: Implement** — append to `fabric/daemon/ops_impl.go`:

```go
// hostDeps builds the hostagent collaborators from the runtime's own fields.
func (d *Daemon) hostDeps() hostagent.Deps {
	return hostagent.Deps{
		Hub: d.hub, Control: d.control, StatePath: d.statePath, MintKey: d.mintKey,
	}
}

// EnsureRoot creates the workspace root on first run, else reuses + re-registers it.
func (d *Daemon) EnsureRoot(ctx context.Context) (agentstate.Agent, error) {
	return hostagent.EnsureRoot(ctx, d.hostDeps())
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestEnsureRootCreatesThenReuses -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /tmp/botbus-impl
git add fabric/daemon/ops_impl.go fabric/daemon/ops_impl_test.go
git commit -m "feat(daemon): Ops.EnsureRoot + hostDeps() helper"
```

---

### Task 4: `CreateChild` op + MCP-first `ConnectInstructions`

**Files:**
- Modify: `fabric/daemon/ops_impl.go`
- Test: `fabric/daemon/ops_impl_test.go`

**Interfaces:**
- Consumes: `hostagent.Create(ctx, Deps, CreateOpts{Name, Focus, Parent})`, `console.RenderWelcome(name, focus, "root", profile)`, `console.SeedWelcome(ctx, hub, inbox, text)`, `d.Addr()`, `d.domain`, `d.profile`.
- Produces: `(*Daemon).CreateChild`.

- [ ] **Step 1: Write the failing test** — append to `ops_impl_test.go`:

```go
func TestCreateChildSeedsWelcomeAndBuildsInstructions(t *testing.T) {
	dir := t.TempDir()
	srv := stubAcceptAll(t)
	defer srv.Close()
	fake := hubclient.NewFake()
	prof := &profile.Profile{User: "Eric", Framing: "we ship",
		Root: profile.Root{ID: "root-id", Key: "root-key", InboxChannel: "rootchan"}}
	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "root-id", Key: "root-key", Name: "root"}},
		Daemon: agentstate.Daemon{MCPAddr: "127.0.0.1:8765"}}
	d := NewRuntime(Config{State: st, StatePath: dir + "/state.json", Hub: fake,
		Control: control.NewClient(srv.URL), Profile: prof,
		MintKey: func() string { return "childkey" }, Domain: "botbus.ai"})

	child, inst, err := d.CreateChild(context.Background(), "botbus-cli", "the CLI")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if child.Parent != "root-id" {
		t.Fatalf("child.Parent=%q want root-id", child.Parent)
	}
	if inst.MCPCommand == "" || inst.MCPEndpoint != "http://127.0.0.1:8765/a/childkey" {
		t.Fatalf("instructions=%+v", inst)
	}
	if inst.ChannelURL != "https://"+child.InboxChannel+".botbus.ai/" {
		t.Fatalf("channelURL=%q", inst.ChannelURL)
	}
	// Welcome was published to the child's inbox channel.
	if got := fake.Published(child.InboxChannel); len(got) == 0 {
		t.Fatalf("no welcome seeded to %s", child.InboxChannel)
	}
}
```

> Note: confirm the fake's published-accessor name. If `hubclient.NewFake()` exposes published messages under a different method (e.g. `Sent`/`Messages`), use that; adjust the assertion to the fake's real API discovered in `go doc github.com/ericpollmann/botbus-proto/hubclient`.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestCreateChildSeeds`
Expected: FAIL — `d.CreateChild` undefined.

- [ ] **Step 3: Implement** — append to `fabric/daemon/ops_impl.go` (add imports `"fmt"` already present; add `"github.com/ericpollmann/botbus-cli/fabric/console"`):

```go
// CreateChild registers a sub-agent under root (mint id + inbox channel +
// register with Parent + seed welcome) and returns MCP-first connect
// instructions. It does NOT spawn a process (see spec Follow-ups).
func (d *Daemon) CreateChild(ctx context.Context, name, focus string) (agentstate.Agent, ConnectInstructions, error) {
	r, err := d.root()
	if err != nil {
		return agentstate.Agent{}, ConnectInstructions{}, err
	}
	child, err := hostagent.Create(ctx, d.hostDeps(), hostagent.CreateOpts{
		Name: name, Focus: focus, Parent: r.ID,
	})
	if err != nil {
		return agentstate.Agent{}, ConnectInstructions{}, fmt.Errorf("create child: %w", err)
	}
	welcome := console.RenderWelcome(child.Name, focus, "root", d.profile)
	if err := console.SeedWelcome(ctx, d.hub, child.InboxChannel, welcome); err != nil {
		return agentstate.Agent{}, ConnectInstructions{}, fmt.Errorf("seed welcome: %w", err)
	}
	endpoint := fmt.Sprintf("http://%s/a/%s", d.Addr(), child.Key)
	return child, ConnectInstructions{
		MCPCommand:  fmt.Sprintf("claude mcp add --transport http %s %s", child.Name, endpoint),
		MCPEndpoint: endpoint,
		ChannelURL:  fmt.Sprintf("https://%s.%s/", child.InboxChannel, d.domain),
	}, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestCreateChildSeeds -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /tmp/botbus-impl
git add fabric/daemon/ops_impl.go fabric/daemon/ops_impl_test.go
git commit -m "feat(daemon): Ops.CreateChild with MCP-first ConnectInstructions"
```

---

### Task 5: `Send` op

**Files:**
- Modify: `fabric/daemon/ops_impl.go`
- Test: `fabric/daemon/ops_impl_test.go`

**Interfaces:**
- Consumes: package `Send(ctx, hub, outboundChannel, from, SendArgs)`, `d.state.Daemon.OutboundChannel`.
- Produces: `(*Daemon).Send`.

- [ ] **Step 1: Write the failing test** — append:

```go
func TestSendPublishesToOutbound(t *testing.T) {
	fake := hubclient.NewFake()
	st := &agentstate.State{Daemon: agentstate.Daemon{OutboundChannel: "outchan"}}
	d := NewRuntime(Config{State: st, Hub: fake})
	if err := d.Send(context.Background(), "root", "hi botbus-cli", []string{"botbus-cli"}, "dm"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := fake.Published("outchan")
	if len(msgs) != 1 {
		t.Fatalf("published=%v", msgs)
	}
}
```

(Adjust `fake.Published` to the fake's real accessor as in Task 4.)

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestSendPublishesToOutbound`
Expected: FAIL — `d.Send` undefined.

- [ ] **Step 3: Implement** — append to `ops_impl.go`:

```go
// Send publishes a message as fromAgent to the daemon's outbound source channel
// (the router routes it). `to` sets the envelope To for direct addressing; kind
// defaults to chat when empty.
func (d *Daemon) Send(ctx context.Context, fromAgent, body string, to []string, kind string) error {
	return Send(ctx, d.hub, d.state.Daemon.OutboundChannel, fromAgent, SendArgs{
		Body: body, To: to, Kind: kind,
	})
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestSendPublishesToOutbound -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /tmp/botbus-impl
git add fabric/daemon/ops_impl.go fabric/daemon/ops_impl_test.go
git commit -m "feat(daemon): Ops.Send wraps envelope publish to outbound"
```

---

### Task 6: `ReadInbox` op

**Files:**
- Modify: `fabric/daemon/ops_impl.go`
- Test: `fabric/daemon/ops_impl_test.go`

**Interfaces:**
- Consumes: package `Next(ctx, *AgentRuntime, timeoutSec) string`, `d.runtimes`.
- Produces: `(*Daemon).ReadInbox`.

- [ ] **Step 1: Write the failing test** — append:

```go
func TestReadInboxUnknownAgentErrors(t *testing.T) {
	d := NewRuntime(Config{State: &agentstate.State{}, Hub: hubclient.NewFake()})
	if _, err := d.ReadInbox(context.Background(), "nope", 1); err == nil {
		t.Fatal("expected error for unknown agent id")
	}
}

func TestReadInboxEmptyOnTimeout(t *testing.T) {
	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "a1", Name: "a"}}}
	d := NewRuntime(Config{State: st, Hub: hubclient.NewFake()})
	out, err := d.ReadInbox(context.Background(), "a1", 1) // 1s, nothing queued
	if err != nil {
		t.Fatalf("ReadInbox: %v", err)
	}
	if out != "[]" {
		t.Fatalf("want empty json array, got %q", out)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestReadInbox`
Expected: FAIL — `d.ReadInbox` undefined.

- [ ] **Step 3: Implement** — append to `ops_impl.go`:

```go
// ReadInbox long-polls one agent's inbox queue (the op behind MCP `next`),
// returning the queued envelopes as a JSON array string. Errors if agentID is
// not a managed runtime.
func (d *Daemon) ReadInbox(ctx context.Context, agentID string, timeoutSec int) (string, error) {
	rt, ok := d.runtimes[agentID]
	if !ok {
		return "", fmt.Errorf("unknown agent id %q", agentID)
	}
	return Next(ctx, rt, timeoutSec), nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestReadInbox -v`
Expected: PASS. Then run the whole package: `go test -race ./fabric/daemon/`.

- [ ] **Step 5: Commit**

```bash
cd /tmp/botbus-impl
git add fabric/daemon/ops_impl.go fabric/daemon/ops_impl_test.go
git commit -m "feat(daemon): Ops.ReadInbox wraps per-agent Next"
```

---

### Task 7: Route the MCP tools through `Ops`

**Files:**
- Modify: `fabric/daemon/mcp.go`, `fabric/daemon/daemon.go` (the `mux` call site)
- Test: `fabric/daemon/mcp_test.go` (add cases; create if absent)

**Interfaces:**
- Consumes: `Ops.Send`, `Ops.ReadInbox`.
- Produces: `agentMCP{ops Ops; agentID, from string}` (replaces direct `rt`/`hub`/`outbound`).

- [ ] **Step 1: Write the failing test** — `fabric/daemon/mcp_test.go`:

```go
package daemon

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// fakeOps records calls so we can assert the tools route through Ops.
type fakeOps struct {
	sentFrom, sentBody string
	sentTo             []string
	readID             string
}

func (f *fakeOps) Roster(context.Context) ([]wireNode, error) { return nil, nil } // see note
func (f *fakeOps) CreateChild(context.Context, string, string) (agentState, ConnectInstructions, error) {
	return agentState{}, ConnectInstructions{}, nil
}
func (f *fakeOps) Send(_ context.Context, from, body string, to []string, kind string) error {
	f.sentFrom, f.sentBody, f.sentTo = from, body, to
	return nil
}
func (f *fakeOps) ReadInbox(_ context.Context, id string, _ int) (string, error) {
	f.readID = id
	return "[]", nil
}
func (f *fakeOps) EnsureRoot(context.Context) (agentState, error) { return agentState{}, nil }

func TestToolSendRoutesThroughOps(t *testing.T) {
	f := &fakeOps{}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"body": "hello", "to": "bob", "kind": "dm"}
	if _, err := ag.toolSend(context.Background(), req); err != nil {
		t.Fatalf("toolSend: %v", err)
	}
	if f.sentFrom != "alice" || f.sentBody != "hello" || len(f.sentTo) != 1 || f.sentTo[0] != "bob" {
		t.Fatalf("ops not called correctly: %+v", f)
	}
}

func TestToolNextRoutesThroughOps(t *testing.T) {
	f := &fakeOps{}
	ag := &agentMCP{ops: f, agentID: "a1", from: "alice"}
	if _, err := ag.toolNext(context.Background(), mcp.CallToolRequest{}); err != nil {
		t.Fatalf("toolNext: %v", err)
	}
	if f.readID != "a1" {
		t.Fatalf("ReadInbox not called with agentID, got %q", f.readID)
	}
}
```

> Note on aliases: the test references `wireNode` / `agentState` only to keep the snippet readable — in the real file use the actual types (`wire.AgentNode`, `agentstate.Agent`) and import them. The `Ops` interface in `ops.go` already uses those real types; `fakeOps` must match the real `Ops` signatures exactly.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestTool`
Expected: FAIL — `agentMCP` has no `ops`/`agentID` fields.

- [ ] **Step 3: Rewire `fabric/daemon/mcp.go`** — change `agentMCP` and the two handlers:

```go
type agentMCP struct {
	ops     Ops
	agentID string
	from    string
}
```

```go
func (ag *agentMCP) toolNext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	timeout := req.GetInt("timeout_seconds", 30)
	if timeout > 300 {
		timeout = 300
	}
	out, err := ag.ops.ReadInbox(ctx, ag.agentID, timeout)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(out), nil
}

func (ag *agentMCP) toolSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var to []string
	if s := req.GetString("to", ""); s != "" {
		to = splitComma(s)
	}
	if err := ag.ops.Send(ctx, ag.from, req.GetString("body", ""), to, req.GetString("kind", "")); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("sent"), nil
}
```

Remove the now-unused `hubclient` import from `mcp.go` if it becomes unused. Update `mux()` in `daemon.go`:

```go
func (d *Daemon) mux() *http.ServeMux {
	m := http.NewServeMux()
	for _, a := range d.state.Agents {
		path := "/a/" + a.Key
		ag := &agentMCP{ops: d, agentID: a.ID, from: a.Name}
		s := buildMCPServer(ag)
		m.Handle(path, server.NewStreamableHTTPServer(s, server.WithEndpointPath(path)))
	}
	return m
}
```

`buildMCPServer`/`buildAgentHandler` keep their signatures (they take `*agentMCP`). If `buildAgentHandler` is now unused, delete it (No Legacy rule).

- [ ] **Step 4: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./fabric/daemon/ -run TestTool -v && go build ./... && go test -race ./fabric/daemon/`
Expected: PASS; existing daemon integration test still green (it now exercises the Ops path).

- [ ] **Step 5: Commit**

```bash
cd /tmp/botbus-impl
git add fabric/daemon/mcp.go fabric/daemon/daemon.go fabric/daemon/mcp_test.go
git commit -m "refactor(daemon): MCP next/send route through Ops"
```

---

### Task 8: Route the console (TUI) through `Ops`

**Files:**
- Create: `cmd/botbus/runtime.go` — `buildRuntime(p) *daemon.Daemon` (the shared runtime constructor; Task 9 extends this file).
- Modify: `cmd/botbus/console.go`
- Test: `cmd/botbus/console_test.go` (add cases)

**Interfaces:**
- Consumes: `daemon.Ops` (`EnsureRoot`, `Roster`, `CreateChild`), `daemon.NewRuntime(Config)`.
- Produces: `buildRuntime(p *profile.Profile) *daemon.Daemon`; `firstRunOps(in, out, ops, profilePath)`, `onboardChildOps(ctx, ops, name, focus)`; `runConsole` builds the runtime once and passes `Ops` to the model.

This task replaces the three direct primitive calls in `console.go` with `Ops` calls. The TUI's `m.onboard` closure returns the **MCP-first** instruction string.

- [ ] **Step 1: Write the failing test** — append to `cmd/botbus/console_test.go`:

```go
func TestOnboardChildOpsReturnsMCPInstruction(t *testing.T) {
	ops := &stubConsoleOps{conn: daemon.ConnectInstructions{
		MCPCommand: "claude mcp add --transport http botbus-cli http://127.0.0.1:8765/a/k",
		ChannelURL: "https://chan.botbus.ai/",
	}}
	msg, err := onboardChildOps(context.Background(), ops, "botbus-cli", "the CLI")
	if err != nil {
		t.Fatalf("onboardChildOps: %v", err)
	}
	if !strings.Contains(msg, "claude mcp add") {
		t.Fatalf("expected MCP-first instruction, got %q", msg)
	}
}
```

Add a `stubConsoleOps` implementing `daemon.Ops` (record args, return the canned `ConnectInstructions`). It must satisfy the full `daemon.Ops` interface (5 methods) — stub the unused ones returning zero values.

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./cmd/botbus/ -run TestOnboardChildOps`
Expected: FAIL — `onboardChildOps` undefined.

- [ ] **Step 3: Implement the Ops-based helpers + rewire** in `cmd/botbus/console.go`:

Replace `onboardChild` body usage with an Ops-based version (keep the old name out — No Legacy):

```go
// onboardChildOps creates a child via the shared Ops core and returns the
// operator-facing connect instruction (MCP-first, channel URL fallback).
func onboardChildOps(ctx context.Context, ops daemon.Ops, name, focus string) (string, error) {
	_, inst, err := ops.CreateChild(ctx, name, focus)
	if err != nil {
		return "", err
	}
	return inst.MCPCommand + "\n(or raw: " + inst.ChannelURL + ")", nil
}
```

In `firstRun`, replace the `hostagent.EnsureRoot(...)` call with `ops.EnsureRoot(ctx)` by threading an `ops daemon.Ops` parameter (rename to `firstRunOps(in, out, ops, profilePath)`; it still reads name/framing and saves the profile, but root creation goes through `ops`). In `runConsole`:

Create `cmd/botbus/runtime.go` with the shared constructor (Task 9 extends this same file):

```go
// buildRuntime constructs the one local-agent runtime shared by the TUI and MCP
// faces. (Task 9 adds ensureSingleRuntime/runAll/RunOn to this file.)
func buildRuntime(p *profile.Profile) *daemon.Daemon {
	st, _ := agentstate.Load(agentstate.DefaultPath())
	return daemon.NewRuntime(daemon.Config{
		State: st, StatePath: agentstate.DefaultPath(),
		Hub:     hubclient.NewHTTPClient(envOr("HUB_BASE", "https://"+domain), envOr("HUB_DOMAIN", domain)),
		Control: control.NewClient(envOr("ROUTER_URL", DefaultRouterURL)),
		Profile: p, MintKey: keys.New, Domain: domain,
	})
}
```

Then rewire `runConsole` in `console.go`:

```go
func runConsole() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	profilePath := profile.DefaultPath()
	p, err := profile.Load(profilePath)
	if err != nil { fmt.Fprintln(os.Stderr, "profile:", err); os.Exit(1) }

	// Build the one runtime (Ops + faces share it). buildRuntime is defined in
	// this task's cmd/botbus/runtime.go (Task 9 adds the single-runtime guard).
	rt := buildRuntime(p)
	if !p.Configured() {
		p, err = firstRunOps(os.Stdin, os.Stdout, rt, profilePath)
		if err != nil { fmt.Fprintln(os.Stderr, "setup:", err); os.Exit(1) }
		rt = buildRuntime(p) // rebuild with the now-populated profile
	}
	nodes, err := rt.Roster(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "roster unavailable (is the router deployed?):", err)
		nodes = []wire.AgentNode{{ID: p.Root.ID, Name: "root", InboxChannel: p.Root.InboxChannel}}
	}
	m := newConsoleModel(nodes)
	wireConsoleChat(ctx, &m, p, rt) // onboard closure now calls onboardChildOps(ctx, rt, ...)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err); os.Exit(1)
	}
}
```

Update `wireConsoleChat` to take `ops daemon.Ops` and set `m.onboard = func(name, focus string) (string, error) { return onboardChildOps(context.Background(), ops, name, focus) }`. Remove the now-dead `onboardChild`/`hostagent` direct import from `console.go` if unused.

- [ ] **Step 4: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./cmd/botbus/ -run TestOnboardChildOps -v && go build ./...`
Expected: PASS; build clean. Keep existing `console_run_test`/`console_test` green.

- [ ] **Step 5: Commit**

```bash
cd /tmp/botbus-impl
git add cmd/botbus/console.go cmd/botbus/console_test.go
git commit -m "refactor(console): TUI onboard/first-run/roster route through Ops"
```

---

### Task 9: All-in-one bootstrap + single-runtime fail-fast

**Files:**
- Modify: `cmd/botbus/runtime.go` (add `ensureSingleRuntime` + `runAll`; `buildRuntime` already added in Task 8)
- Modify: `cmd/botbus/main.go` (dispatch), `cmd/botbus/daemon.go` (use `NewRuntime`), `fabric/daemon/daemon.go` (add `RunOn`)
- Test: `cmd/botbus/runtime_test.go`

**Interfaces:**
- Consumes: `daemon.NewRuntime(Config)`, `daemon.Daemon.Run(ctx)`, `daemon.Daemon.Addr()`.
- Produces: `buildRuntime(p *profile.Profile) *daemon.Daemon`; `runAll(ctx, rt, withTUI bool)`; a port-bind preflight `ensureSingleRuntime(addr string) (net.Listener, error)`.

- [ ] **Step 1: Write the failing test** — `cmd/botbus/runtime_test.go`:

```go
package main

import (
	"net"
	"testing"
)

func TestEnsureSingleRuntimeFailsWhenPortBusy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	defer ln.Close()
	addr := ln.Addr().String() // already bound

	if _, err := ensureSingleRuntime(addr); err == nil {
		t.Fatal("expected fail-fast when the runtime port is already bound")
	}
}

func TestEnsureSingleRuntimeSucceedsWhenFree(t *testing.T) {
	l2, err := ensureSingleRuntime("127.0.0.1:0")
	if err != nil { t.Fatalf("expected success on a free port: %v", err) }
	l2.Close()
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /tmp/botbus-impl && go test ./cmd/botbus/ -run TestEnsureSingleRuntime`
Expected: FAIL — `ensureSingleRuntime` undefined.

- [ ] **Step 3: Add `ensureSingleRuntime` + `runAll` to `cmd/botbus/runtime.go`** (the file `buildRuntime` already lives in from Task 8; add `"context"`, `"fmt"`, `"net"`, `"os"` to its imports as needed)

```go
// ensureSingleRuntime is the per-host mutex: it binds the runtime's MCP port so
// a second runtime (e.g. `botbus daemon` while `botbus` is open) fails fast
// instead of double-subscribing every inbox and colliding on the port.
func ensureSingleRuntime(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("a botbus runtime is already running on %s — stop it first", addr)
	}
	return ln, nil
}

// runAll serves the runtime's faces — inbox loops + per-agent MCP mux — on the
// pre-bound listener, so the single-runtime port-mutex is held continuously
// from preflight through serve. The TUI, when present, is run by the caller
// (runConsole) alongside this. Uses RunOn (Step 4) to serve on a bound listener.
func runAll(ctx context.Context, rt *daemon.Daemon, ln net.Listener) error {
	return rt.RunOn(ctx, ln)
}
```

- [ ] **Step 4: Add `RunOn` to `fabric/daemon/daemon.go`** so the preflight listener is reused (avoids a bind/serve TOCTOU). Refactor `Run` to bind then delegate:

```go
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
	g.Go(func() error { <-gctx.Done(); return srv.Close() })
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
```

Add `"net"` to `daemon.go` imports.

- [ ] **Step 5: Wire dispatch in `cmd/botbus/main.go` and `daemon.go`**

In `runConsole` (Task 8), before launching the TUI, take the port: `ln, err := ensureSingleRuntime(rt.Addr())` (fail-fast on error), start `go runAll(ctx, rt, ln)` for the inbox/MCP faces, then run the TUI. In `daemonCmd` (cmd/botbus/daemon.go), build via `buildRuntime(nil)` (headless: no operator profile), `ln, err := ensureSingleRuntime(rt.Addr())`, then `rt.RunOn(ctx, ln)`.

- [ ] **Step 6: Run to verify it passes**

Run: `cd /tmp/botbus-impl && go test ./cmd/botbus/ -run TestEnsureSingleRuntime -v && go build ./... && go vet ./... && go test -race ./...`
Expected: PASS; full suite green.

- [ ] **Step 7: Commit**

```bash
cd /tmp/botbus-impl
git add cmd/botbus/runtime.go cmd/botbus/runtime_test.go cmd/botbus/main.go cmd/botbus/daemon.go fabric/daemon/daemon.go
git commit -m "feat(botbus): all-in-one runtime (TUI+MCP) with single-runtime fail-fast"
```

---

## Self-review checklist (run before requesting review)

- [ ] **Spec coverage:** `Ops` (T1) · Roster (T2) · EnsureRoot (T3) · CreateChild + MCP-first instructions (T4) · Send (T5) · ReadInbox (T6) · MCP routed through Ops (T7) · TUI routed through Ops (T8) · all-in-one + single-runtime (T9). Deferred items untouched. ✓
- [ ] **Type consistency:** `Ops` method signatures identical across `ops.go`, `ops_impl.go`, `mcp.go` (agentMCP), the console stub, and the `fakeOps` test double. `ConnectInstructions` fields (`MCPCommand`/`MCPEndpoint`/`ChannelURL`) used identically everywhere.
- [ ] **Fake API:** confirm `hubclient.NewFake()`'s published-message accessor name via `go doc`; fix `fake.Published(...)` calls in Tasks 4–5 to match.
- [ ] **No legacy:** old `onboardChild` and `buildAgentHandler` removed if unused; no compat shims.
- [ ] **Green gate:** `go build ./... && go vet ./... && go test -race ./...` before each commit.

## Open items for the implementer to verify against live code (not assumptions)
- The exact `hubclient` fake accessor (see above).
- `runPresence` signature (used unchanged in `RunOn`) — confirm it matches the current `Run`.
- Whether any existing daemon test constructs `agentMCP{rt:..., hub:..., outbound:...}` directly; if so, update it to the new `{ops, agentID, from}` shape in the same task that changes the struct (Task 7).
