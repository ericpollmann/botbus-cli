# E2E Fail-Closed + Local Welcome Injection — Implementation Report

## Changes Made

### 1. `fabric/daemon/inbox.go` — Fail-closed opener for e2e agents

In `openerFor`, the e2e branch previously returned `(e, true)` for `Enc==""` frames
(tolerated cleartext). Changed to return `(envelope.Envelope{}, false)` — drop
unconditionally. Updated comment to explain the fail-closed decision and that the
welcome is delivered locally. Non-e2e branch unchanged (passes cleartext, drops `Enc!=""`).

### 2. `fabric/daemon/ops_impl.go` — Local welcome injection for e2e workspaces

Added `envelope` import. In `CreateChild`:
- Added workspace lookup via `d.state.WorkspaceFor(child.Parent)` (using Parent not
  child.ID, because the child is not yet in d.state.Agents before `d.attach(child)` runs).
- For non-e2e workspaces: existing `console.SeedWelcome` hub path unchanged.
- For e2e workspaces: skip `SeedWelcome`; after `d.attach(child)` call `d.injectLocal`.

Added `injectLocal(agentID string, e envelope.Envelope)` helper at end of file:
- Takes `d.mu` to read `d.runtimes[agentID]`, then releases mu before calling
  `rt.enqueue(e)`. This matches the daemon's convention (enqueue acquires its own
  `rt.mu`; holding `d.mu` across it would invert the lock order). No `d.mu` held
  across `enqueue`.

The injected envelope: `{V:1, ID:envelope.NewID(), From:"botbus", Kind:KindChat, Body:welcome}`.

### 3. `fabric/daemon/e2e_integration_test.go` — Flip pinned test

Renamed `TestE2EAgentDropsCleartextIsAcceptedAsPassthroughToday` →
`TestE2EAgentDropsCleartextFrame`. Changed assertion: expects `ok == false`.
Updated doc comment to state the fail-closed decision (no longer pending).

### 4. `fabric/daemon/ops_impl_test.go` — Local welcome injection test

Added `TestCreateChildE2EInjectsWelcomeLocally`. Harness mirrors the existing
`TestCreateChildSeedsWelcomeAndBuildsInstructions` but adds an e2e workspace
(`Workspaces: []agentstate.Workspace{{RootID:"root-id", E2E:true, Epoch:1}}`).

Asserts:
- (a) `fake.Published(child.InboxChannel)` is empty — no hub publish.
- (b) `d.runtimes[child.ID].drain()` has one envelope with `From=="botbus"`,
  `Kind=="chat"`, non-empty `Body`.

End-to-end coverage notes: `CreateChild` drives the full `hostagent.Create` +
`d.attach` + `d.injectLocal` path. The daemon is not "serving" in this test
(no `RunOn`), so `attach` adds the runtime but does not start inbox loops — which
is correct and sufficient for verifying injection. The hub-path suppression is
directly observable via the fake hub's Published counter.

### 5. `README.md` — E2E limitations note

Added bullet: "Fail-closed inbound filtering — e2e agents reject all unencrypted
inbound frames; the connect welcome is delivered locally for e2e workspaces."

## Verification Output

### `go test ./... -count=1`
```
ok  github.com/ericpollmann/botbus-cli/cmd/botbus        0.893s
ok  github.com/ericpollmann/botbus-cli/fabric/agentstate 0.370s
ok  github.com/ericpollmann/botbus-cli/fabric/console    2.428s
ok  github.com/ericpollmann/botbus-cli/fabric/control    1.738s
ok  github.com/ericpollmann/botbus-cli/fabric/daemon     7.096s
ok  github.com/ericpollmann/botbus-cli/fabric/e2e        1.060s
ok  github.com/ericpollmann/botbus-cli/fabric/hostagent  2.123s
ok  github.com/ericpollmann/botbus-cli/fabric/profile    1.371s
```

### `go test ./fabric/daemon/ -race -count=1`
```
ok  github.com/ericpollmann/botbus-cli/fabric/daemon  5.848s
```

### `go vet ./...`
No output (clean).

### `go build ./...`
No output (clean).

### `git status --porcelain`
Empty after commit.

## What could not be driven end-to-end

`CreateChild` end-to-end was fully exercised in the unit test harness (including
`hostagent.Create` against a stub HTTP control server, `d.attach`, and
`d.injectLocal`). The one aspect not exercised is a live daemon serving HTTP
requests where the e2e welcome arrives via `ReadInbox` through the full MCP
`next` tool path — this would require `RunOn` with a live listener, which the
existing integration test (`TestCreateChildServesEndpointWhileRunning`) does not
cover the e2e welcome case. This is acceptable: `injectLocal` calls `rt.enqueue`
which is the same path used by `runInbox`, and that path is covered by runtime
tests. The injection test directly asserts the runtime queue.
