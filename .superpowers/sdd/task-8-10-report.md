# Tasks 8–10: Cross-Host Admit/Join Methods + Integration Test

## Methods implemented

### `(d *Daemon).AdmitJoinRequest(ctx, ws, req) (AdmitGrant, error)`
- File: `fabric/daemon/admission.go`
- Builds new anchor set (current + joiner), signs with admin key (seed-stored AdminPriv via ed25519.NewKeyFromSeed)
- Applies anchor set locally via `d.trust.applyAnchorSet`
- Publishes sealed anchor frame to roster channel
- Wraps workspace key to joiner's EncPub (NaCl anonymous box)
- Publishes JSON grant to WaitingRoom

### `ProcessAdmitGrant(grant, encPriv) (*Workspace, [32]byte, bool)`
- File: `fabric/daemon/admission.go`
- Unwraps workspace key from grant.WrappedKey using joiner's EncPriv
- Returns populated Workspace on success, (nil, {}, false) on failure

### `(d *deviceSet).snapshot() map[string][]byte`
- File: `fabric/daemon/deviceset.go`
- Returns copy of id→pubBytes map under RLock

## Integration test: `TestCrossHostJoinAdmitConverge`
- File: `fabric/daemon/cross_host_admission_test.go`
- **Relay-blind**: grant frame on wire does not contain plaintext workspace key; WrappedKey non-empty
- **Key recovery**: joiner recovers exact workspace key via ProcessAdmitGrant
- **Convergence**: after admission, admin daemon opens joiner's sealed message (joiner-1 in anchor set)
- **Reject**: intruder (never admitted, no cert) dropped by admin opener
- **Roster**: anchors frame published, admin-signed blob names joiner-1

## API mismatch adapted
- AdminPriv stored as 32-byte Ed25519 seed (workspace.go:67 `adminPrivKey.Seed()`); used `ed25519.NewKeyFromSeed(ws.AdminPriv)` to derive full private key

## Test results
```
=== RUN   TestCrossHostJoinAdmitConverge
--- PASS: TestCrossHostJoinAdmitConverge (0.03s)
PASS
ok  	github.com/ericpollmann/botbus-cli/fabric/daemon	1.602s
```
Full suite (all daemon tests, -race): PASS (6.2s, 0 failures)

## Commit
[fill in after committing]
