# Final Security Fixes Report

## FIX I-1 — Authenticate AdmitGrant

**Files changed:** `fabric/daemon/admission.go`, `fabric/daemon/cross_host_admission_test.go`

### What was done
- Added `Sig []byte` field (`json:"sig,omitempty"`) to `AdmitGrant`.
- Added `grantSignedPayload(g AdmitGrant) []byte` — domain-tagged, length-prefixed canonical payload over all grant fields EXCEPT `Sig` itself. Format: `"botbus-e2e-grant-v1\x00"` + LP(ReqID, AnchorID, RootID) + Epoch(uint32-LE) + LP(WrappedKey, AdminPub, Roster, WaitingRoom).
- `AdmitJoinRequest` now signs the grant with `ed25519.Sign(ed25519.NewKeyFromSeed(ws.AdminPriv), grantSignedPayload(grant))` before marshaling/publishing.
- `ProcessAdmitGrant` signature changed to `(grant AdmitGrant, encPriv []byte, expectedAdminPub []byte)`. It now rejects if: `expectedAdminPub` is empty; `grant.AdminPub` doesn't match `expectedAdminPub`; or the Ed25519 signature fails verification.
- Updated the one caller in `cross_host_admission_test.go` to pass `adminPub`.

### New test: `TestProcessAdmitGrantRejectsForgedGrant`
Assertions:
- (a) `grant.AdminPub` swapped to attacker's key → rejected (AdminPub mismatch)
- (b) `grant.WrappedKey` tampered post-signing → rejected (signature invalid)
- (b2) `grant.RootID` tampered post-signing → rejected (signature invalid)
- (c) Wrong `expectedAdminPub` passed → rejected
- (c2) Empty `expectedAdminPub` → rejected
- Baseline with valid grant + correct `expectedAdminPub` → accepted

---

## FIX M-2 — Epoch-Monotonic Anchor Set

**Files changed:** `fabric/daemon/deviceset.go`, `fabric/daemon/deviceset_test.go`

### What was done
- In `deviceSet.applySigned`: after verifying the admin signature and parsing, added: `if parsed.Epoch < d.epoch { return fmt.Errorf("...") }`. Strictly-less check — equal epoch is allowed (idempotent re-apply). Mutation only happens on success.

### New test: `TestApplySignedRejectsStaleEpoch`
Assertions:
- Apply epoch-2 blob with `dev-2` → succeeds.
- Apply validly-admin-signed epoch-1 blob with `dev-1` → returns error.
- After rejection: `dev-2` (epoch-2) is still present; `dev-1` (epoch-1 only) is NOT present.

---

## Test Run (`-race -count=1 -timeout 90s`)

All 70 tests in `fabric/daemon` PASS including both new tests.
`go vet ./...` and `go build ./...` clean.

## Commit

Commit hash: (see git log)
`fix(e2e): authenticate AdmitGrant + epoch-monotonic anchor set`

Git status after commit: clean.

---

# Round-2 Concurrent-Persist & CLI Hardening Fixes

## I-1 — BLOCKER: concurrent unlocked marshal of d.state (data race on state.json)

### Root cause

`persistWorkspaceKey` called `agentstate.Save(d.statePath, d.state)` without
holding `d.mu`. Both `applyRekey` and `recordPending` release `d.mu`, then call
`persistWorkspaceKey`. When two goroutines do this concurrently, the JSON encoder
reads `d.state` (including `ws.Key`, `ws.Pending`, etc.) while the other goroutine
is mutating it under `d.mu`, producing a classic read/write data race.

### Fix

`persistWorkspaceKey` now acquires `d.mu` around the `agentstate.Save(...)` call
and releases it before logging. Callers (`applyRekey`, `recordPending`) were
verified to release `d.mu` BEFORE calling `persistWorkspaceKey` — the call is
outside the lock in both cases. The doc comment was updated to document the
"Callers must NOT hold d.mu" invariant.

**File:** `fabric/daemon/daemon.go` (~line 379)

### Race regression test — `fabric/daemon/persist_race_test.go`

`TestPersistWorkspaceKeyNoRace`:
- Builds a `*Daemon` with a real `statePath` (t.TempDir()) and an e2e workspace
  with one seeded anchor.
- Spawns two goroutines, each running 250 iterations:
  - G1: `d.applyRekey(ws, randKey, epoch)` — writes ws.Key/ws.Epoch under lock, then persists.
  - G2: `d.recordPending(ws, req)` — appends ws.Pending under lock, then persists.
- Both goroutines `wg.Wait()` and the test passes.

**RED (pre-fix):** the race detector reports a concurrent read/write on `d.state` fields.
**GREEN (post-fix):** no race detected; test passes in ~2s.

---

## M-1 — read ws.Key/ws.Epoch under the lock in e2eContextFor (Send path)

**File:** `fabric/daemon/e2ectx.go`

`e2eContextFor` previously read `ws.Key` (via `keyArray(ws.Key)`) and `ws.Epoch`
without holding `d.mu`, racing `applyRekey`'s locked write. The fix copies both
fields under `d.mu` (Lock → copy bytes out + read epoch → Unlock) before using
them in the returned `e2eCtx`.

---

## M-2 — move test-only accessors out of production code

**Files:** `fabric/daemon/waitingroom_loop.go` (removed), `fabric/daemon/testsupport_test.go` (created)

`pendingLen` and `pendingReqID` were test helpers compiled into the production
binary. Moved verbatim to `testsupport_test.go` (package `daemon`). All
waiting-room tests (`TestRunWaitingRoomRecordsPending`) continue to pass.

---

## M-3 — collapse dead length-check branch

**File:** `fabric/daemon/daemon.go` (`hydrateWorkspaceTrust`)

`len(ar.SignPub) == ed25519.SeedSize || len(ar.SignPub) == ed25519.PublicKeySize`
is `32 || 32` — always the same constant. Replaced with a single
`len(ar.SignPub) == ed25519.PublicKeySize` check with a one-line comment
explaining why the OR was dead.

---

## M-4 — guard admin CLI ops on ws.E2E

**File:** `cmd/botbus/workspace.go`

`workspaceAdmit`, `workspaceKeyRotate`, and `workspaceRemove` now check
`ws.E2E` after resolving the workspace, returning
`"workspace %q is not end-to-end encrypted"` if false.

**Test:** `TestWorkspaceAdmitNonE2ERejectsWithGuard` in
`cmd/botbus/workspace_admit_test.go` — saves a plaintext (E2E=false) workspace,
calls `workspaceAdmit`, expects a non-nil error containing
"not end-to-end encrypted".

---

## M-5 — admit success output

**File:** `cmd/botbus/workspace.go`

`workspaceAdmit` signature changed from `error` to `(int, error)` — the `int`
is the anchor count after admission. `workspaceCmd` now prints:
`"admitted %s (workspace now has %d anchor(s))\n"`. Existing
`TestWorkspaceAdmit` updated to assert `anchorCount == 1`.

---

## Test Run (`-race -count=1 -timeout=120s`)

```
ok  github.com/ericpollmann/botbus-cli/fabric/daemon  8.784s
FAIL github.com/ericpollmann/botbus-cli/cmd/botbus    1.196s
--- FAIL: TestNameColor (0.00s)   ← pre-existing failure, acceptable
```

All `fabric/daemon` tests pass with the race detector. The only failure is the
pre-existing `TestNameColor` in `cmd/botbus`.

---

## Commits

- `9e6bbb0` — `fix(e2e): marshal state under d.mu on persist; lock e2eContextFor key read (race)`
  - I-1, M-1, M-2 (test move), M-3 (dead branch), persist_race_test.go, testsupport_test.go
- `ec38d76` — `chore(e2e): E2E guard on admin CLI ops; admit count output; tidy test helpers + dead branch`
  - M-4 (E2E guard + test), M-5 (anchor count output)
