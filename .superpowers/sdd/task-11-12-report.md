# Task 11 + 12 Report — Key Rotation and Anchor Removal

## Summary

Implemented `RotateKey` (Task 11) and `RemoveAnchor` (Task 12) on `*Daemon` in
`fabric/daemon/lifecycle.go`, following TDD: tests were written first in
`lifecycle_test.go`, confirmed to fail (compile error: undefined methods), then
production code was written to make them pass.

---

## Data-Model Addition: `anchorEnc` Map

**Problem:** `deviceSet` (the anchor trust store) maps anchorID → Ed25519 _sign_
pubkey only. Key rotation must re-wrap the new key to each anchor's X25519 _enc_
pubkey, which is not stored there.

**Solution:** Added `anchorEnc map[string][]byte` to `Daemon` (guarded by
`d.mu`). It maps anchorID → 32-byte X25519 enc pubkey and is populated in
`AdmitJoinRequest` when `req.EncPub` is valid (len == 32).

**In-memory caveat (v1):** `anchorEnc` is not persisted to disk. Anchors that
were admitted in a _prior_ daemon process will not have their enc-pub recorded,
so `RotateKey` will silently omit their rekey frames after a restart. Persisting
`anchorEnc` alongside workspace state is the planned v2 enhancement.

---

## `rosterFrame` Extension

Added two optional fields to `fabric/daemon/roster.go`:

```go
WrappedKey []byte `json:"wrappedKey,omitempty"` // NaCl-sealed new key, Kind=="rekey"
AnchorID   string `json:"anchorId,omitempty"`   // target anchor for this rekey frame
```

The `"rekey"` kind carries only the wrapped (sealed) key — the new key never
appears in plaintext on the wire (relay-blind).

---

## `deviceSet.remove` Helper

Added to `fabric/daemon/deviceset.go`:

```go
func (d *deviceSet) remove(id string) // drops id under Lock; no-op if absent
```

---

## `RotateKey` (Task 11) — `fabric/daemon/lifecycle.go`

Steps:
1. `crypto/rand.Read` → 32-byte `newKey`; `newEpoch = ws.Epoch + 1`.
2. Update `ws.Key = newKey[:]`, `ws.Epoch = newEpoch`.
3. Re-marshal `signedDeviceSet{Epoch: newEpoch, Devices: d.trust.anchors.snapshot()}`,
   admin-sign with `ws.AdminPriv`, call `applyAnchorSet` locally, publish an
   `"anchors"` roster frame sealed under the new key.
4. For each anchorID in `d.anchorEnc`: `wrapKey(newKey, encPub)` → publish a
   `"rekey"` roster frame (Kind, WrappedKey, AnchorID) sealed under the new key.
5. Return `newKey`. Old key is not deleted (callers may hold a retention window).

---

## `RemoveAnchor` (Task 12) — `fabric/daemon/lifecycle.go`

Steps:
1. `d.trust.anchors.remove(anchorID)` — evicts from trust graph under Lock.
2. `delete(d.anchorEnc, anchorID)` — ensures no rekey frame is ever sent to it.
3. Call `d.RotateKey(ctx, ws)` — issues a new epoch; the removed anchor receives
   neither the anchors frame nor any rekey frame.

---

## Test Assertions — `fabric/daemon/lifecycle_test.go`

### `TestRotateKeyReWrapsToAnchors`

- Admits `"anchor-1"` via `AdmitJoinRequest` (records enc-pub).
- Calls `RotateKey`.
- **(a)** `ws.Epoch == oldEpoch+1`; `newKey != oldKey`; `ws.Key == newKey[:]`.
- **(b)** Relay-blind: plaintext and base64 of new key absent from all published
  messages; a `"rekey"` frame addressed to `"anchor-1"` exists and its
  `WrappedKey` unwraps (via the anchor's `encPriv`) to exactly `newKey`.
- **(c)** `"anchor-1"` is present in the `"anchors"` frame's blob at the new epoch.

### `TestRemoveAnchorEvicts`

- Admits `"anchor-keep"` and `"anchor-remove"`.
- Calls `RemoveAnchor(…, "anchor-remove")`.
- **(a)** `ws.Epoch > epochBeforeRemove`.
- **(b)** `"anchor-remove"` absent from new anchor blob; `"anchor-keep"` present.
- **(c)** No `"rekey"` frame addressed to `"anchor-remove"`; `"anchor-keep"`
  receives a rekey frame.
- `d.trust.anchors.lookup("anchor-remove")` returns false.
- `d.trust.resolve("anchor-remove")` returns false (messages from it are dropped
  by any opener, since the resolver is the gate in `e2e.OpenMessage`).
- `d.trust.resolve("anchor-keep")` returns true (no collateral damage).
- Relay-blind: plaintext new key absent from all published messages.

---

## Verification

```
go test ./fabric/daemon/ -race -count=1 -timeout 90s
ok      github.com/ericpollmann/botbus-cli/fabric/daemon    6.281s

go vet ./...   → clean
go build ./... → clean
gofmt -w       → clean (no diff)
```

---

## Files Changed

| File | Change |
|------|--------|
| `fabric/daemon/lifecycle.go` | NEW — `RotateKey`, `RemoveAnchor` |
| `fabric/daemon/lifecycle_test.go` | NEW — `TestRotateKeyReWrapsToAnchors`, `TestRemoveAnchorEvicts` |
| `fabric/daemon/roster.go` | Add `WrappedKey`, `AnchorID` fields + `"rekey"` kind comment |
| `fabric/daemon/deviceset.go` | Add `(*deviceSet).remove` helper |
| `fabric/daemon/daemon.go` | Add `anchorEnc` field, initialize in `NewRuntime`, update `mu` comment |
| `fabric/daemon/admission.go` | Record `req.EncPub` in `anchorEnc` after successful admit |

---

## In-Memory Caveat (v1)

`anchorEnc` is not persisted. After a daemon restart, rotation cannot re-wrap to
anchors admitted in prior sessions. This is acceptable for v1 — the spec
explicitly calls it out as out-of-scope. Track as a future enhancement alongside
workspace-state serialization.
