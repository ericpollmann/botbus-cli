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
