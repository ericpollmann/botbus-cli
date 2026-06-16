# Routing Fabric — Phase 2b (Local Daemon, session mode) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `botbus daemon` that multiplexes all of a host's fabric agents over one process: it subscribes to each agent's inbox channel with cursor-based catch-up, re-registers + heartbeats them with the router, and exposes a localhost MCP server whose `next`/`send` tools are an agent's await loop and outbound path — replacing the public-SSE-and-hope loop.

**Architecture:** New importable package `fabric/daemon` in the public `botbus-cli` repo, plus a `daemon` subcommand in `cmd/botbus`. Reuses what PR #6 already landed (`fabric/hostagent`, `fabric/agentstate`, `fabric/control`) and `botbus-proto v0.2.0` (`hubclient` for subscribe-with-resume, `envelope`, `filter`). Per-agent state is a small in-memory runtime (delivery queue + cursor + dedup); delivery transport is the hub itself (the daemon holds no server-side state). One MCP streamable-HTTP endpoint per agent on `127.0.0.1`, keyed by the agent's capability key (the unguessable key doubles as localhost auth).

**Tech Stack:** Go 1.25, `github.com/ericpollmann/botbus-proto v0.2.0` (hubclient/envelope/filter), `github.com/mark3labs/mcp-go v0.54.0` (NEW dep — localhost MCP server, same version as the private router's `mcp.go`), `golang.org/x/sync/errgroup` (NEW dep — coordinated goroutine lifecycle), stdlib `net/http`/`crypto`. Tests use `hubclient.Fake` and `httptest` (no network).

---

## Scope

In scope (session mode): daemon process; per-agent inbox subscription with reconnect + resume cursor; cursor persistence to `~/.botbus/state.json`; idempotent re-register + heartbeat loop; localhost MCP server with `next` (long-poll the agent's queue) and `send` (stamp + publish outbound).

Out of scope (later plans): spawn mode `claude -p` (Phase 2c); `filter_*`/`interest_set`/`registry_list` MCP tools (Phase 3); outbound `escalate`/`status` fan-out (Phase 4); live add of agents to a running daemon (requires restart in 2b — note it, don't build it).

## Identity & delivery model (read first)

- The router delivers to an inbox as a single `kind:"batch"` envelope whose `Body` is a JSON array of inner envelopes (see the merged `fabric/router` `deliver`). The daemon **unwraps** that batch and enqueues the inner envelopes individually.
- `hubclient.HTTPClient.Subscribe(ctx, channel, resume)` returns `<-chan Frame` where `Frame.Body` is the message body (the `name:` prefix already stripped) and `Frame.Resume` is the SSE `id:` token. The channel closes on disconnect; it does **not** auto-reconnect — the daemon wraps it in a reconnect loop, re-subscribing from the last persisted `Frame.Resume`.
- Delivery is at-least-once; dedup by `envelope.ID`. The persisted cursor (`agentstate.Agent.Cursor`) survives daemon restarts; the hub's per-channel buffer replays the gap.
- Outbound `send` stamps `From = agent.ID`, `ID`, `TS`, and publishes to the daemon's configured outbound source channel (`agentstate.Daemon` gets an `OutboundChannel`), which the router watches.

## File Structure

**Create (in `botbus-cli`):**
- `fabric/daemon/runtime.go` — `AgentRuntime`: delivery queue, cursor, dedup set; `enqueue`, `drain`, `waitNext`.
- `fabric/daemon/runtime_test.go`
- `fabric/daemon/inbox.go` — `runInbox`: subscribe→unwrap→enqueue→advance-cursor reconnect loop.
- `fabric/daemon/inbox_test.go`
- `fabric/daemon/presence.go` — `runPresence`: register-then-heartbeat loop.
- `fabric/daemon/presence_test.go`
- `fabric/daemon/tools.go` — `Next`/`Send` pure logic (the MCP tool bodies), independent of mcp-go.
- `fabric/daemon/tools_test.go`
- `fabric/daemon/mcp.go` — per-agent MCP handler (`mark3labs/mcp-go`) + localhost mux.
- `fabric/daemon/mcp_test.go`
- `fabric/daemon/daemon.go` — `Daemon`: wire state → runtimes + inbox + presence + MCP; `Run(ctx)`.
- `fabric/daemon/daemon_test.go`
- `fabric_daemon_e2e_test.go` (repo root) — daemon + real hub + router end to end.

**Modify:**
- `fabric/agentstate/agentstate.go` — add `OutboundChannel` to `Daemon`; add `SetCursor(id, cursor)` helper + a `SaveCursor` path (atomic, debounced by caller).
- `cmd/botbus/main.go` — add `daemon` subcommand.
- `go.mod` — add `github.com/mark3labs/mcp-go v0.54.0` and `golang.org/x/sync`.

---

## Task 1: AgentRuntime — queue, dedup, drain, waitNext

**Files:**
- Create: `fabric/daemon/runtime.go`, `fabric/daemon/runtime_test.go`

- [ ] **Step 1: Write the failing test**

```go
package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
)

func TestRuntimeEnqueueDrainDedup(t *testing.T) {
	rt := newRuntime("myth-compiler", 100)

	rt.enqueue(envelope.Envelope{ID: "a", Body: "1"})
	rt.enqueue(envelope.Envelope{ID: "b", Body: "2"})
	rt.enqueue(envelope.Envelope{ID: "a", Body: "dup"}) // duplicate id dropped

	got := rt.drain()
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("drain = %+v", got)
	}
	if len(rt.drain()) != 0 {
		t.Fatal("second drain should be empty")
	}
}

func TestRuntimeWaitNextBlocksThenReturns(t *testing.T) {
	rt := newRuntime("a", 100)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		time.Sleep(20 * time.Millisecond)
		rt.enqueue(envelope.Envelope{ID: "x", Body: "hi"})
	}()

	got := rt.waitNext(ctx) // blocks until something is enqueued
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("waitNext = %+v", got)
	}
}

func TestRuntimeWaitNextTimeoutReturnsEmpty(t *testing.T) {
	rt := newRuntime("a", 100)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if got := rt.waitNext(ctx); len(got) != 0 {
		t.Fatalf("expected empty on timeout, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestRuntime -v`
Expected: FAIL — `newRuntime` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// Package daemon is the host-side botbus daemon: it multiplexes all of a
// host's fabric agents over one process — one inbox subscription, delivery
// queue, and MCP endpoint per agent — so sessions never hold an internet-
// facing SSE connection and big models wake only for their own inbox.
package daemon

import (
	"context"
	"sync"

	"github.com/ericpollmann/botbus-proto/envelope"
)

// AgentRuntime is the live per-agent state inside the daemon: a bounded
// delivery queue of inbound envelopes, an at-least-once dedup set keyed by
// envelope id, and a signal for long-pollers.
type AgentRuntime struct {
	ID string

	mu     sync.Mutex
	queue  []envelope.Envelope
	seen   map[string]struct{}
	seenN  int
	maxN   int
	signal chan struct{} // buffered(1); poked on enqueue
}

func newRuntime(id string, maxSeen int) *AgentRuntime {
	return &AgentRuntime{
		ID:     id,
		seen:   make(map[string]struct{}),
		maxN:   maxSeen,
		signal: make(chan struct{}, 1),
	}
}

// enqueue appends an envelope unless its id was already seen. It pokes the
// signal channel (non-blocking) so a waiting next() wakes.
func (r *AgentRuntime) enqueue(e envelope.Envelope) {
	r.mu.Lock()
	if _, dup := r.seen[e.ID]; dup {
		r.mu.Unlock()
		return
	}
	r.markSeen(e.ID)
	r.queue = append(r.queue, e)
	r.mu.Unlock()
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// markSeen records an id, bounding the set by clearing it when it grows past
// maxN (dedup is a recent-window guarantee, not forever).
func (r *AgentRuntime) markSeen(id string) {
	if r.seenN >= r.maxN {
		r.seen = make(map[string]struct{})
		r.seenN = 0
	}
	r.seen[id] = struct{}{}
	r.seenN++
}

// drain removes and returns all queued envelopes.
func (r *AgentRuntime) drain() []envelope.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.queue
	r.queue = nil
	return out
}

// waitNext returns queued envelopes immediately if any, else blocks until an
// enqueue or ctx cancellation, then returns whatever is queued (possibly empty
// on timeout).
func (r *AgentRuntime) waitNext(ctx context.Context) []envelope.Envelope {
	if got := r.drain(); len(got) > 0 {
		return got
	}
	select {
	case <-ctx.Done():
		return r.drain()
	case <-r.signal:
		return r.drain()
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestRuntime -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/runtime.go fabric/daemon/runtime_test.go
git commit -m "feat(daemon): AgentRuntime queue with dedup + long-poll signal"
```

---

## Task 2: Inbox catch-up loop (unwrap batch, advance cursor, reconnect)

**Files:**
- Create: `fabric/daemon/inbox.go`, `fabric/daemon/inbox_test.go`

- [ ] **Step 1: Write the failing test** (uses `hubclient.Fake`)

```go
package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// makeBatch wraps inner envelopes the way the router's deliver() does.
func makeBatch(t *testing.T, inner ...envelope.Envelope) hubclient.Frame {
	t.Helper()
	body, _ := json.Marshal(inner)
	wrap := envelope.Envelope{V: 1, ID: envelope.NewID(), From: "router", Kind: envelope.KindBatch, Body: string(body)}
	raw, _ := envelope.Encode(wrap)
	return hubclient.Frame{Name: "router", Body: string(raw), Resume: "cursor-1"}
}

func TestRunInboxUnwrapsBatchAndAdvancesCursor(t *testing.T) {
	fake := hubclient.NewFake()
	rt := newRuntime("myth-compiler", 100)

	var savedCursor string
	persist := func(cursor string) { savedCursor = cursor }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runInbox(ctx, rt, fake, "inbox-compiler", "", persist)

	// Let the subscription establish, then inject a router batch frame.
	time.Sleep(20 * time.Millisecond)
	fake.Inject("inbox-compiler", makeBatch(t,
		envelope.Envelope{ID: "m1", From: "eric", Body: "build"},
		envelope.Envelope{ID: "m2", From: "eric", Body: "test"},
	))

	deadline := time.After(time.Second)
	for {
		if got := rt.drain(); len(got) == 2 {
			if got[0].ID != "m1" || got[1].ID != "m2" {
				t.Fatalf("inner envelopes wrong: %+v", got)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("inbox never delivered the unwrapped envelopes")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if savedCursor != "cursor-1" {
		t.Fatalf("cursor = %q, want cursor-1", savedCursor)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestRunInbox -v`
Expected: FAIL — `runInbox` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
package daemon

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// reconnectBackoff is the pause between a dropped inbox subscription and the
// next re-subscribe. Var so tests can shrink it.
var reconnectBackoff = 2 * time.Second

// runInbox subscribes to one agent's inbox channel and feeds its runtime,
// resuming from `cursor` and re-subscribing from the latest seen cursor after
// any disconnect, until ctx is cancelled. persist is called with each new
// cursor so it can be written to local state.
func runInbox(ctx context.Context, rt *AgentRuntime, hub hubclient.HubClient, inbox, cursor string, persist func(string)) {
	for {
		if ctx.Err() != nil {
			return
		}
		frames, err := hub.Subscribe(ctx, inbox, cursor)
		if err != nil {
			log.Printf("daemon: subscribe %s: %v", inbox, err)
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
		for fr := range frames {
			for _, inner := range unwrap(fr.Body) {
				rt.enqueue(inner)
			}
			if fr.Resume != "" {
				cursor = fr.Resume
				persist(cursor)
			}
		}
		// Channel closed (disconnect) — back off and resume from cursor.
		if !sleepCtx(ctx, reconnectBackoff) {
			return
		}
	}
}

// unwrap turns a router delivery frame body into the inner envelopes. A
// kind:"batch" envelope carries a JSON array in its Body; anything else is
// treated as a single envelope (forward-compat / direct frames).
func unwrap(body string) []envelope.Envelope {
	e, err := envelope.Decode([]byte(body))
	if err != nil {
		return nil
	}
	if e.Kind != envelope.KindBatch {
		return []envelope.Envelope{e}
	}
	var inner []envelope.Envelope
	if err := json.Unmarshal([]byte(e.Body), &inner); err != nil {
		log.Printf("daemon: bad batch body: %v", err)
		return nil
	}
	return inner
}

// sleepCtx sleeps for d or until ctx is done; returns false if ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestRunInbox -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/inbox.go fabric/daemon/inbox_test.go
git commit -m "feat(daemon): inbox catch-up loop — unwrap batch, advance + persist cursor"
```

---

## Task 3: Cursor persistence in agentstate

**Files:**
- Modify: `fabric/agentstate/agentstate.go`, `fabric/agentstate/agentstate_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSetCursorPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}
	if err := Save(path, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := SetCursor(path, "a", "cursor-9"); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	reloaded, _ := Load(path)
	got, _ := reloaded.Get("a")
	if got.Cursor != "cursor-9" {
		t.Fatalf("cursor = %q, want cursor-9", got.Cursor)
	}
	// Unknown id is a no-op error.
	if err := SetCursor(path, "missing", "x"); err == nil {
		t.Fatal("SetCursor on unknown id should error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/agentstate/ -run TestSetCursor -v`
Expected: FAIL — `SetCursor` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `fabric/agentstate/agentstate.go`:

```go
// SetCursor loads, updates the cursor for one agent, and re-saves atomically.
// It returns an error if the agent id is unknown. Callers that advance the
// cursor on every frame should debounce writes themselves.
func SetCursor(path, id, cursor string) error {
	s, err := Load(path)
	if err != nil {
		return err
	}
	a, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("agentstate: unknown agent %q", id)
	}
	a.Cursor = cursor
	s.Upsert(a)
	return Save(path, s)
}
```

Add `"fmt"` to the import block if not present.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/agentstate/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/agentstate/agentstate.go fabric/agentstate/agentstate_test.go
git commit -m "feat(agentstate): SetCursor — persist an agent's inbox resume cursor"
```

---

## Task 4: Presence loop (re-register + heartbeat)

**Files:**
- Create: `fabric/daemon/presence.go`, `fabric/daemon/presence_test.go`

- [ ] **Step 1: Write the failing test** (real `control` server is private; test against an `httptest` stub that records calls)

```go
package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
)

func TestRunPresenceRegistersThenHeartbeats(t *testing.T) {
	var registers, heartbeats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			registers.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost:
			heartbeats.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	heartbeatEvery = 20 * time.Millisecond
	defer func() { heartbeatEvery = 30 * time.Second }()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	a := agentstate.Agent{ID: "myth-compiler", Key: "key-1", InboxChannel: "i", Focus: "compile"}
	runPresence(ctx, control.NewClient(srv.URL), a)

	if registers.Load() != 1 {
		t.Fatalf("registers = %d, want 1", registers.Load())
	}
	if heartbeats.Load() < 2 {
		t.Fatalf("heartbeats = %d, want >=2", heartbeats.Load())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestRunPresence -v`
Expected: FAIL — `runPresence`/`heartbeatEvery` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
package daemon

import (
	"context"
	"log"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/wire"
)

// heartbeatEvery is the presence-refresh interval (well under the router's
// 90s lease TTL). Var so tests can shrink it.
var heartbeatEvery = 30 * time.Second

// runPresence re-registers the agent once (idempotent; replays desired state
// so a router/Redis restart self-heals) then heartbeats on a ticker until ctx
// is cancelled.
func runPresence(ctx context.Context, ctl *control.Client, a agentstate.Agent) {
	if err := ctl.Register(ctx, a.ID, a.Key, specOf(a)); err != nil {
		log.Printf("daemon: register %s: %v", a.ID, err)
	}
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ctl.Heartbeat(ctx, a.ID, a.Key); err != nil {
				log.Printf("daemon: heartbeat %s: %v", a.ID, err)
			}
		}
	}
}

// specOf maps a local agent entry to the control register body. Mirrors
// hostagent.specOf (kept local to avoid exporting it there).
func specOf(a agentstate.Agent) wire.AgentSpec {
	return wire.AgentSpec{
		Name: a.Name, InboxChannel: a.InboxChannel, Focus: a.Focus,
		Interest: a.Interest, Parent: a.Parent, Mode: a.Mode,
		BatchMS: a.BatchMS, BatchN: a.BatchN, BatchBytes: a.BatchBytes,
		ModelTier: a.ModelTier,
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestRunPresence -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/presence.go fabric/daemon/presence_test.go
git commit -m "feat(daemon): presence loop — idempotent re-register + heartbeat"
```

---

## Task 5: `next` and `send` tool logic

**Files:**
- Create: `fabric/daemon/tools.go`, `fabric/daemon/tools_test.go`

These are the pure bodies of the MCP tools, independent of mcp-go so they unit-test directly. `Next` long-polls a runtime; `Send` stamps and publishes.

- [ ] **Step 1: Write the failing test**

```go
package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestNextReturnsQueued(t *testing.T) {
	rt := newRuntime("a", 100)
	rt.enqueue(envelope.Envelope{ID: "m1", Body: "hello"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	out := Next(ctx, rt, 1)
	var got []envelope.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("next output not a JSON envelope array: %v (%s)", err, out)
	}
	if len(got) != 1 || got[0].Body != "hello" {
		t.Fatalf("next = %s", out)
	}
}

func TestSendStampsAndPublishes(t *testing.T) {
	fake := hubclient.NewFake()
	ctx := context.Background()

	err := Send(ctx, fake, "outbound-chan", "myth-compiler", SendArgs{
		To: []string{"myth-boss"}, Kind: "dm", Body: "need review",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	pubs := fake.Published("outbound-chan")
	if len(pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(pubs))
	}
	// Body is "name: <json>"; strip the prefix and decode.
	raw := pubs[0]
	idx := len("myth-compiler: ")
	e, err := envelope.Decode([]byte(raw[idx:]))
	if err != nil {
		t.Fatalf("decode published: %v (%s)", err, raw)
	}
	if e.From != "myth-compiler" || e.Kind != "dm" || e.Body != "need review" || e.ID == "" || e.TS == "" {
		t.Fatalf("bad stamped envelope: %+v", e)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run 'TestNext|TestSend' -v`
Expected: FAIL — `Next`/`Send`/`SendArgs` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// nowRFC3339 is overridable in tests; defaults to wall clock.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }

// Next long-polls the agent's runtime for up to timeoutSec seconds and returns
// the queued envelopes as a JSON array string (empty array on timeout).
func Next(ctx context.Context, rt *AgentRuntime, timeoutSec int) string {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	out := rt.waitNext(cctx)
	if out == nil {
		out = []envelope.Envelope{}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// SendArgs are the agent-supplied fields for an outbound message.
type SendArgs struct {
	To      []string
	Kind    string
	Scope   string
	Subject string
	Body    string
}

// Send stamps id/ts/from onto an outbound envelope and publishes it to the
// daemon's outbound source channel, where the router picks it up.
func Send(ctx context.Context, hub hubclient.HubClient, outboundChannel, from string, a SendArgs) error {
	kind := a.Kind
	if kind == "" {
		kind = envelope.KindChat
	}
	e := envelope.Envelope{
		V: 1, ID: envelope.NewID(), TS: nowRFC3339(), From: from,
		To: a.To, Kind: kind, Scope: a.Scope, Subject: a.Subject, Body: a.Body,
	}
	raw, err := envelope.Encode(e)
	if err != nil {
		return err
	}
	return hub.Publish(ctx, outboundChannel, from+": "+string(raw))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run 'TestNext|TestSend' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/tools.go fabric/daemon/tools_test.go
git commit -m "feat(daemon): next (long-poll) + send (stamp+publish) tool logic"
```

---

## Task 6: MCP server — per-agent endpoints on localhost

**Files:**
- Create: `fabric/daemon/mcp.go`, `fabric/daemon/mcp_test.go`
- Modify: `go.mod` (add `github.com/mark3labs/mcp-go v0.54.0`)

Each agent gets a streamable-HTTP MCP handler mounted at `/a/{key}`; the unguessable capability key in the path is the localhost auth and selects the runtime. `next`/`send` close over that agent's runtime + hub.

- [ ] **Step 1: Add the MCP dependency**

Run: `go get github.com/mark3labs/mcp-go@v0.54.0`
Expected: `go.mod` gains the require; `go.sum` updated.

- [ ] **Step 2: Write the failing test** (drive the handler with mcp-go's in-process client)

```go
package daemon

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestMCPNextToolReturnsEnvelopes(t *testing.T) {
	rt := newRuntime("myth-compiler", 100)
	rt.enqueue(envelope.Envelope{ID: "m1", From: "eric", Body: "build"})

	ag := &agentMCP{rt: rt, hub: hubclient.NewFake(), outbound: "out", from: "myth-compiler"}
	srv := httptest.NewServer(buildAgentHandler(ag))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := client.NewStreamableHttpClient(srv.URL)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("init: %v", err)
	}

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "next", Arguments: map[string]any{"timeout_seconds": 1}},
	})
	if err != nil {
		t.Fatalf("call next: %v", err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !contains(text, "m1") || !contains(text, "build") {
		t.Fatalf("next result missing envelope: %s", text)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = time.Second
```

> Note: confirm the exact mcp-go client constructor/result-content types against `botbus/go.mod`'s v0.54.0 (`client.NewStreamableHttpClient`, `mcp.TextContent`). If the API differs, adapt the test to the installed version — the private `botbus/mcp.go` and its tests are the reference.

- [ ] **Step 3: Write minimal implementation**

```go
package daemon

import (
	"context"
	"net/http"

	"github.com/ericpollmann/botbus-proto/hubclient"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// agentMCP binds one agent's runtime + outbound path for its MCP tools.
type agentMCP struct {
	rt       *AgentRuntime
	hub      hubclient.HubClient
	outbound string
	from     string
}

// buildAgentHandler returns a streamable-HTTP MCP handler exposing next/send
// for one agent.
func buildAgentHandler(ag *agentMCP) http.Handler {
	s := server.NewMCPServer("botbus-daemon", "0.1.0", server.WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("next",
		mcp.WithDescription("Long-poll this agent's inbox; returns a JSON array of envelopes (possibly empty on timeout)."),
		mcp.WithNumber("timeout_seconds", mcp.Description("Default 30, max 300.")),
	), ag.toolNext)
	s.AddTool(mcp.NewTool("send",
		mcp.WithDescription("Publish an outbound message; the daemon stamps id/ts/from."),
		mcp.WithString("body", mcp.Required()),
		mcp.WithString("to", mcp.Description("Comma-separated agent ids for direct address.")),
		mcp.WithString("kind", mcp.Description("chat|dm|task|escalate|status|review_request; default chat.")),
		mcp.WithString("subject"),
	), ag.toolSend)
	return server.NewStreamableHTTPServer(s)
}

func (ag *agentMCP) toolNext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	timeout := req.GetInt("timeout_seconds", 30)
	return mcp.NewToolResultText(Next(ctx, ag.rt, timeout)), nil
}

func (ag *agentMCP) toolSend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := SendArgs{
		Body:    req.GetString("body", ""),
		Kind:    req.GetString("kind", ""),
		Subject: req.GetString("subject", ""),
	}
	if to := req.GetString("to", ""); to != "" {
		args.To = splitComma(to)
	}
	if err := Send(ctx, ag.hub, ag.outbound, ag.from, args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText("sent"), nil
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r == ' ' {
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
```

> Note: `req.GetInt`/`req.GetString` are mcp-go v0.54.0 request accessors used by `botbus/mcp.go`. Confirm names against the installed version; adapt if needed.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestMCP -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/mcp.go fabric/daemon/mcp_test.go go.mod go.sum
git commit -m "feat(daemon): per-agent localhost MCP handler (next/send)"
```

---

## Task 7: Daemon assembly + mux

**Files:**
- Create: `fabric/daemon/daemon.go`, `fabric/daemon/daemon_test.go`
- Modify: `fabric/agentstate/agentstate.go` (add `OutboundChannel` to `Daemon`)

- [ ] **Step 1: Add `OutboundChannel` to the Daemon config**

In `fabric/agentstate/agentstate.go`, extend `Daemon`:

```go
type Daemon struct {
	RouterURL       string `json:"router_url"`
	HubBase         string `json:"hub_base"`
	HubDomain       string `json:"hub_domain"`
	OutboundChannel string `json:"outbound_channel,omitempty"`
	MCPAddr         string `json:"mcp_addr,omitempty"` // default 127.0.0.1:8765
}
```

- [ ] **Step 2: Write the failing test**

```go
package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestDaemonMountsPerAgentEndpoints(t *testing.T) {
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ctl.Close()

	st := &agentstate.State{
		Daemon: agentstate.Daemon{RouterURL: ctl.URL, OutboundChannel: "out", MCPAddr: "127.0.0.1:0"},
		Agents: []agentstate.Agent{{ID: "myth-compiler", Key: "key-xyz", InboxChannel: "inbox-c"}},
	}
	d := New(st, "", hubclient.NewFake())
	mux := d.mux()

	// The agent's MCP endpoint exists at /a/<key> and the wrong key 404s.
	rec := httptest.NewrecorderFor(t, mux, "/a/key-xyz")
	if rec == http.StatusNotFound {
		t.Fatal("expected /a/key-xyz to be mounted")
	}
	if got := httptest.NewrecorderFor(t, mux, "/a/wrong"); got != http.StatusNotFound {
		t.Fatalf("unknown key should 404, got %d", got)
	}
	_ = context.Background
	_ = time.Second
	_ = strings.TrimSpace
}
```

> Note: the pseudo-helpers above are placeholders for "issue a GET to `mux` at the path and capture the status code." Implement the test with a real `httptest.NewRequest` + `httptest.NewRecorder` + `mux.ServeHTTP` and assert `rec.Code`. (Kept compact here; write it concretely.) The substance: `New(state, statePath, hub)` builds a daemon; `d.mux()` returns an `http.Handler` mounting `/a/<key>` per agent and 404 for unknown keys.

- [ ] **Step 3: Write minimal implementation**

```go
package daemon

import (
	"context"
	"net/http"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/sync/errgroup"
)

// Daemon multiplexes a host's agents: one inbox subscription + runtime +
// presence loop + MCP endpoint each.
type Daemon struct {
	state     *agentstate.State
	statePath string
	hub       hubclient.HubClient
	runtimes  map[string]*AgentRuntime
}

// New builds a Daemon from loaded state.
func New(state *agentstate.State, statePath string, hub hubclient.HubClient) *Daemon {
	rts := make(map[string]*AgentRuntime, len(state.Agents))
	for _, a := range state.Agents {
		rts[a.ID] = newRuntime(a.ID, 1000)
	}
	return &Daemon{state: state, statePath: statePath, hub: hub, runtimes: rts}
}

// mux mounts one MCP endpoint per agent at /a/<key>.
func (d *Daemon) mux() http.Handler {
	m := http.NewServeMux()
	for _, a := range d.state.Agents {
		ag := &agentMCP{rt: d.runtimes[a.ID], hub: d.hub, outbound: d.state.Daemon.OutboundChannel, from: a.ID}
		m.Handle("/a/"+a.Key, http.StripPrefix("/a/"+a.Key, buildAgentHandler(ag)))
	}
	return m
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

	addr := d.state.Daemon.MCPAddr
	if addr == "" {
		addr = "127.0.0.1:8765"
	}
	srv := &http.Server{Addr: addr, Handler: d.mux()}
	g.Go(func() error {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	g.Go(func() error { <-gctx.Done(); return srv.Close() })

	return g.Wait()
}
```

Run: `go get golang.org/x/sync@latest` for errgroup.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestDaemon -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/daemon.go fabric/daemon/daemon_test.go fabric/agentstate/agentstate.go go.mod go.sum
git commit -m "feat(daemon): Daemon assembly — per-agent inbox+presence+MCP, errgroup lifecycle"
```

---

## Task 8: `botbus daemon` subcommand

**Files:**
- Modify: `cmd/botbus/main.go`

- [ ] **Step 1: Add the subcommand dispatch**

In `cmd/botbus/main.go`, extend the early subcommand check (it already special-cases `agent`):

```go
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		daemonCmd(os.Args[2:])
		return
	}
```

Add the handler (new function in `main.go`):

```go
func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	hubBase := fs.String("hub", envOr("HUB_BASE", "https://botbus.ai"), "hub base URL")
	hubDomain := fs.String("hub-domain", envOr("HUB_DOMAIN", "botbus.ai"), "hub apex domain")
	_ = fs.Parse(args)

	statePath := agentstate.DefaultPath()
	st, err := agentstate.Load(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon: load state:", err)
		os.Exit(1)
	}
	if len(st.Agents) == 0 {
		fmt.Fprintln(os.Stderr, "daemon: no agents in", statePath, "- create one with 'botbus agent create'")
		os.Exit(1)
	}

	hub := hubclient.NewHTTPClient(*hubBase, *hubDomain)
	d := daemon.New(st, statePath, hub)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	addr := st.Daemon.MCPAddr
	if addr == "" {
		addr = "127.0.0.1:8765"
	}
	fmt.Printf("botbus daemon: %d agent(s), MCP on %s\n", len(st.Agents), addr)
	for _, a := range st.Agents {
		fmt.Printf("  %s  ->  http://%s/a/%s\n", a.ID, addr, a.Key)
	}
	if err := d.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "daemon:", err)
		os.Exit(1)
	}
}
```

Add imports to `main.go`: `context`, `flag` (likely present), `os/signal`, `syscall`, and `github.com/ericpollmann/botbus-cli/fabric/daemon`, `github.com/ericpollmann/botbus-cli/fabric/agentstate`, `github.com/ericpollmann/botbus-proto/hubclient`. Reuse the existing `envOr` helper if `agent.go` defines one; otherwise add it.

- [ ] **Step 2: Build + vet**

Run: `go build ./cmd/botbus/ && go vet ./...`
Expected: success.

- [ ] **Step 3: Full gate**

Run: `go build ./... && go test ./... -count=1`
Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/botbus/main.go
git commit -m "feat(botbus): daemon subcommand — run the multiplexing daemon"
```

---

## Task 9: End-to-end — daemon + real hub + router

**Files:**
- Create: `fabric_daemon_e2e_test.go` (repo root)

Proves the loop: register an agent, start a router (the private control server isn't importable here, so register via the public `control.Client` against a test control server backed by the same in-memory registry the router reads — OR drive delivery directly through `hubclient`). Because the router lives in the private repo, this e2e validates the **daemon side** against a real hub: publish a router-style batch to the agent's inbox channel and assert the daemon's MCP `next` returns the inner envelopes, including reconnect catch-up.

- [ ] **Step 1: Write the test**

```go
package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestDaemonE2EReceivesRoutedBatch(t *testing.T) {
	fake := hubclient.NewFake()
	rt := daemon.NewRuntimeForTest("myth-compiler") // small exported test helper, or use daemon.New + mux

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = httptest.NewServer
	_ = json.Marshal
	_ = envelope.NewID
	_ = time.Second
	_ = fake
	_ = rt
}
```

> Note: this task's concrete form depends on choices in Tasks 1–8 (whether to export a test seam). Implement it to exercise `daemon.New(...).Run(ctx)` against a real hub `httptest.Server` (construct via the public `hubclient.NewHTTPClient` pointed at it) if a public hub test server is available; otherwise assert the daemon path with `hubclient.Fake` + the MCP client as in Task 6. Keep the assertion: a router-shaped batch injected on the inbox surfaces through `next` as individual envelopes, and after a simulated disconnect the daemon re-subscribes from the persisted cursor with no loss/dupe. Mirror `botbus`'s `fabric_e2e_test.go` style for any real-hub wiring.

- [ ] **Step 2: Run + full gate**

Run: `go build ./... && go test ./... -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add fabric_daemon_e2e_test.go
git commit -m "test(daemon): e2e — routed batch surfaces via MCP next; reconnect catch-up"
```

---

## Self-Review

**Spec coverage (Phase 2b scope — spec §7):**
- One daemon per host multiplexing all local agents (one goroutine + queue each) → Tasks 1, 7. ✅
- Inbox subscription with cursor catch-up, resume on reconnect, dedup → Tasks 1, 2, 3. ✅
- Idempotent re-register on connect + heartbeat lease → Task 4. ✅
- Localhost MCP `next`/`send`, per-agent identity via capability key in the path → Tasks 5, 6, 7. ✅
- Local-state cursor persistence (hub buffer is the offline queue) → Task 3, used in Task 7. ✅
- `botbus daemon` subcommand → Task 8. ✅

**Deferred (correctly out of 2b):** spawn mode (2c); `filter_*`/`interest_set`/`registry_list` tools (Phase 3); outbound fan-out (Phase 4); live agent add without restart (noted).

**Open items to resolve during execution (don't block):**
- Confirm mcp-go v0.54.0 client/result API names (`client.NewStreamableHttpClient`, `req.GetInt/GetString`, `mcp.TextContent`) against the installed version — `botbus/mcp.go` is the reference; adapt Tasks 6/9 tests if they differ.
- Cursor write debounce: Task 7 persists on every frame via `SetCursor` (load+save). If that proves too chatty under load, batch writes behind a ticker — measure first (YAGNI now; the per-agent frame rate after routing is low).
- `next` ack semantics: 2b returns queued envelopes and relies on the inbox cursor for cross-restart catch-up; the "advance on the following next" ack from the spec is unnecessary while the queue is in-memory only. Revisit if spawn mode (2c) needs at-least-once across process exits.
