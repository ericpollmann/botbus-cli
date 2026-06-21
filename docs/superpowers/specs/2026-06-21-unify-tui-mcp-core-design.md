# Unify the TUI and MCP over one local-agent core

- **Date:** 2026-06-21
- **Status:** Design approved in dialogue; awaiting written-spec review → implementation plan
- **Repo:** `github.com/ericpollmann/botbus-cli`

## Problem

The botbus CLI has two front-ends that each reach into the lower-level libraries
themselves:

- The **TUI** (`botbus` no-args — `cmd/botbus/console.go`, `ui.go`) drives roster,
  dip-in, and onboard by calling `hostagent.Create` / `control.Roster` directly.
- The **MCP daemon** (`botbus daemon` — `fabric/daemon`) holds its own runtime and
  exposes per-agent `next` / `send` MCP tools.

This invites **divergent code paths**: "create a child," "list the roster," and
"send / dm" can each get implemented twice (once for the operator's TUI, once for
the agent-facing MCP), and the two can drift in behavior.

Separately, the operator-facing actions we want next — `add` (create a sub-agent),
`list` (roster), `dm` (addressed send) — should be **real operations against the
actual functionality**, not raw slash-text broadcast over the channel. Today's
console `/dm` is display-only sugar (`protocol.go:renderSlash`) that never sets the
envelope `To`, so the router never routes it.

## Goals

- A **single core** ("the local-agent runtime") that owns all agent state and
  operations, with thin front-ends over it. One implementation, no divergence.
- `add` / `list` / `dm` become **real operations** invoked from the TUI (and
  available to MCP), not chat text on the wire.
- Keep the boundary clean enough that a **third front-end (HTTP + React)** drops in
  later by wrapping the same interface — without a rewrite.

## Non-goals (deferred — see Follow-ups)

- Spawning a live *process* for a created child (v1 registers only).
- `dm` privacy enforcement at the hub (stays envelope metadata).
- The HTTP/React web face.
- TUI-as-network-client / auto-attach to an already-running daemon.
- Teaching the hub-served recipe (server-side `ui.go`) about the MCP option.

## Architecture

**One runtime core, thin faces over a Go ops-interface (in-process).** This is
mostly *promoting and consolidating* what `fabric/daemon` already is.

```
            ┌──────────────── core: local-agent runtime ────────────────┐
            │  owns: agentstate.State (agents, keys, channels, cursors)  │
            │        one hubclient.HubClient  +  control.Client          │
            │        per-agent inbox loops  +  MCP mux                    │
            │  exposes Ops:                                              │
            │     Roster() · CreateChild() · Send() · ReadInbox()        │
            │     · EnsureRoot()                                         │
            └──────▲────────────────────▲────────────────────▲──────────┘
                   │ Go calls           │ Go calls            │ HTTP (later)
            ┌──────┴──────┐      ┌───────┴───────┐     ┌───────┴───────┐
            │  TUI face   │      │  MCP face     │     │  Web face     │
            │ console/ui  │      │ daemon mux    │     │ (deferred)    │
            │ roster/dip/ │      │ /a/<key>:     │     │ HTTP+React    │
            │ add         │      │ next · send   │     │               │
            └─────────────┘      └───────────────┘     └───────────────┘
```

- **Boundary** = a Go package API (the `Ops` interface). The future web face wraps
  the same interface in HTTP; that is the only place an HTTP boundary appears, and
  it is deferred.
- All front-ends are thin: they translate input → an `Ops` call → render the result.

## The `Ops` interface

```go
// Ops is the single surface every front-end (TUI, MCP, future HTTP) calls.
// Implemented by the local-agent Runtime.
type Ops interface {
    // Roster returns the agent tree (parent links + liveness).
    Roster(ctx context.Context) ([]wire.AgentNode, error)

    // CreateChild registers a sub-agent under the operator's root (mint id +
    // inbox channel + register with Parent + seed welcome) and returns the new
    // agent plus how to connect to it. Does NOT spawn a process (see Follow-ups).
    CreateChild(ctx context.Context, name, focus string) (agentstate.Agent, ConnectInstructions, error)

    // Send publishes a message as `fromAgent`. `to` (optional) sets the envelope
    // To for router-level addressing (the real "dm"); `kind` defaults to chat.
    Send(ctx context.Context, fromAgent, body string, to []string, kind string) error

    // ReadInbox returns buffered messages for an agent from a cursor (the
    // operation behind MCP `next` and the TUI dip-in read).
    ReadInbox(ctx context.Context, agentID, cursor string) ([]Message, string, error)

    // EnsureRoot creates the operator's root if absent (first-run), else returns it.
    EnsureRoot(ctx context.Context, p *profile.Profile) (agentstate.Agent, error)
}
```

`ConnectInstructions` is **MCP-first when daemon-hosted**, with the raw recipe as
fallback:

```go
type ConnectInstructions struct {
    MCPCommand  string // e.g. `claude mcp add --transport http <name> http://127.0.0.1:8765/a/<key>`
    MCPEndpoint string // http://127.0.0.1:<port>/a/<key>
    ChannelURL  string // https://<inbox>.botbus.ai/  (raw curl-recipe fallback)
}
```

## Components & refactor map (mostly relocation)

| Existing | Becomes |
|---|---|
| `hostagent.Create` / `CreateRoot` | low-level primitives **called by** `Ops.CreateChild` / `Ops.EnsureRoot` |
| `daemon` mux `next` / `send` handlers | thin adapters → `Ops.ReadInbox` / `Ops.Send` |
| `console.go` `onboardChild`, roster fetch, dip-in | TUI actions → `Ops.CreateChild` / `Ops.Roster` / (`ReadInbox` + `Send`) |
| `cmd/botbus` entry points | shared bootstrap builds the `Runtime` once; selects which faces to enable |

The concrete `Runtime` owns `agentstate.State`, the one `hubclient.HubClient`, the
`control.Client`, the per-agent inbox loops, and the MCP mux, and implements `Ops`.

## Entry points (shared bootstrap)

- `botbus` (no-args) → build `Runtime`; start inbox loops + MCP mux **and** attach
  the TUI. **One process, both faces** — the operator's console *is* the host.
- `botbus daemon` → same `Runtime`, headless (MCP only) — for hosts with no
  terminal.

## Single runtime per host (the all-in-one consequence)

A `Runtime` owns the inbox subscriptions and binds the localhost MCP port. Running
`botbus` (no-args) *and* `botbus daemon` simultaneously would **double-subscribe
every inbox** (the "two subscribers double-fire" rule) and collide on the port.

**v1 rule: the port bind is the mutex.** If a runtime is already running, the
second one **fails fast** with a clear message, e.g.:

> `a botbus runtime is already running on :8765 — stop it first`

No auto-attach in v1 (that would require the TUI to become a network client — the
boundary deliberately deferred to the web-UI phase).

## Data flow (key operations)

- **add** → TUI captures name + focus → `Ops.CreateChild` → mint id, mint inbox
  channel, register with `Parent=root`, seed welcome → returns
  `ConnectInstructions` → TUI shows the MCP-first connect line. Inbox loop for the
  new agent starts in the runtime.
- **list** → TUI / MCP → `Ops.Roster` → `control.Roster(root.id, key)` → render.
- **dm / send** → `Ops.Send(from, body, to=[name], kind)` → envelope with `To` set
  → the router's existing direct-addressing routes it to the target inbox (vs.
  today's display-only `/dm`).
- **dip-in** (TUI) → `Ops.ReadInbox(agentID, cursor)` to render history + live, and
  `Ops.Send` to post; not a new op.
- **next** (MCP) → `Ops.ReadInbox` for the calling agent.

## Error handling

- `CreateChild` is the multi-step mint→register→seed chain; on any step failure it
  returns a wrapped error and the front-end surfaces it (no partial "ghost" agent
  left registered without a usable channel — mirror the existing idempotent
  first-run handling in `hostagent`).
- Second-runtime start → fail fast (see Single runtime per host).
- `Send` with an unroutable `to` (no such agent / not live) still publishes to the
  channel buffer; addressing is best-effort at the router (documented, not an
  error).

## Testing strategy (the payoff of one core)

- **Test the ops once, in the core.** Unit-test `Runtime`/`Ops`
  (`CreateChild`, `Roster`, `Send`, `ReadInbox`, `EnsureRoot`) with the existing
  fakes (`fakeHub`, miniredis-backed registry, fake `control.Client`). Target 100%
  coverage on the new core package.
- **Front-ends stay thin → test only the wiring** (MCP handler calls the right
  `Ops` method; TUI action calls the right `Ops` method). No double-testing of
  logic.
- **`ConnectInstructions`**: assert MCP-first command + channel-URL fallback.
- **Single-runtime rule**: assert a second `Runtime` bind fails fast with the
  message.
- Keep existing suites green (`console_run_test`, `daemon` integration) as the
  relocation regression net.

## Follow-ups (deferred — tracked here)

These are intentionally out of scope for this spec; recorded so they aren't lost:

1. **Spawn live child processes.** `CreateChild` registers only; nothing launches
   an actual agent, which is why onboarded agents show `○` idle. A later phase
   could have the runtime spawn/supervise child agent processes (or hand off a
   launch command).
2. **`dm` privacy at the hub.** Today `to`/`dm` is envelope metadata on a public
   channel; the hub broadcasts everything. A later phase could add real private
   delivery.
3. **HTTP + React web face.** The third front-end — wrap `Ops` in a localhost HTTP
   API and serve a React UI (like the page the hub serves on a channel).
4. **TUI-as-network-client / auto-attach.** Let `botbus` attach to an
   already-running daemon instead of failing fast — needs the HTTP boundary from
   #3.
5. **Hub-served recipe learns MCP.** Update server-side `ui.go`
   `renderInstructionsText` so the `curl -s https://<channel>/` recipe also
   mentions the MCP gateway option (a server-repo change).

## Dependencies

- Builds on the merged console (`botbus-cli` PR #11) — now on `main`.
- The router's direct-addressing (`To` field) and the daemon's `send` `to`/`kind`
  already exist; `Ops.Send` rides them.
