# Task 3 Report: trustGraph

## Status: COMPLETE

**Commit:** `93a2337` — `feat(e2e): trust graph (admitted anchors + cert chains)`

## Files Created

- `fabric/daemon/trustgraph.go` — `trustGraph` struct + `newTrustGraph`, `addCert`, `applyAnchorSet`, `resolve`
- `fabric/daemon/trustgraph_test.go` — 7 tests covering all required scenarios

## Implementation Notes

- `resolve` takes `g.mu.RLock()` once at the top, snapshots `g.certs` as a local reference, then releases. The recursive helper `resolveChain` never re-acquires `g.mu`, so no deadlock.
- `anchors.lookup` acquires the deviceSet's own mutex — no lock-ordering issue with `g.mu` already released.
- Cycle/depth safety: a `visited map[string]bool` is threaded through `resolveChain`. Any revisited node returns `(nil, false)` immediately.
- Max recursion depth is naturally bounded by the size of the visited set (at most `len(certs)+1` unique ids).

## Test Results

```
=== RUN   TestTrustGraphDirectAnchor        PASS
=== RUN   TestTrustGraphOneHopChild         PASS
=== RUN   TestTrustGraphTwoHopGrandchild    PASS
=== RUN   TestTrustGraphRejectsUnanchored   PASS
=== RUN   TestTrustGraphRejectsBadSig       PASS
=== RUN   TestTrustGraphRejectsTamperedChildPub  PASS
=== RUN   TestTrustGraphCycleSafe           PASS  (terminated in <1ms, not hung)
PASS  ok  fabric/daemon  1.625s  (-race)
```

Full `go test ./fabric/daemon/ -count=1`: PASS (4.746s).
`go vet ./...`: clean. `go build ./...`: clean. `gofmt -l`: no output (clean).
`git status --porcelain`: empty.

## Concerns

None. The pre-existing `cmd/botbus TestNameColor` failure on `main` is unrelated and was not touched.

---

## Security Review Fixes (applied after initial implementation)

**Fix 1 — Comment correctness (`trustgraph.go` line 45):**
Replaced misleading comment `// local reference; map entries are immutable once stored` with accurate:
`// snapshot the map reference under RLock; the lock (not immutability) excludes concurrent addCert during this read pass`

**Fix 2 — `TestTrustGraphRejectsSelfSignedCert`:**
Builds a trustGraph with NO anchor for "X". Creates a self-signed cert (`ChildID == ParentID == "X"`, signed with X's own key). Asserts `resolve("X")` returns `(nil, false)` — a node cannot bootstrap trust by anchoring itself.

**Fix 3 — `TestTrustGraphAnchorWinsOverCert`:**
Admits "A" (with `aPub`) and "evil" (with `evilPub`) in a single anchor-set call. Adds a forged cert with `ChildID="A"` claiming `evilPub` (signed by "evil"). Asserts `resolve("A")` returns `aPub` not `evilPub` — the anchor check in `resolveChain` (step 1) short-circuits before the cert path (step 2+).
Note: `buildAnchorSet` helper replaces the whole map on each call (epoch semantics); the test uses a single `marshalDeviceSet`+`applyAnchorSet` call with both devices to avoid eviction.

**Test command output:**
```
ok  	github.com/ericpollmann/botbus-cli/fabric/daemon	1.581s
```
All 9 TestTrustGraph* tests pass (including the 2 new ones), `-race`, `go vet ./fabric/daemon/`: clean, `go build ./...`: clean.
