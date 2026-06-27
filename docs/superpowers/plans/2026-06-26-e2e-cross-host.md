# E2E Cross-Host Admission — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Let agents on **different hosts** join one e2e workspace and converge — via per-node admission, subtree trust over parent-signed certificate chains, a waiting-room join flow, sealed-box key wrapping, and rotate-on-membership-change — all daemon-to-daemon and self-verified with two-daemon tests over a shared fake hub.

**Architecture:** Generalizes the merged single-host core. The flat admin-signed device set becomes an **admitted-anchor set** (admin-signed) plus a **cert pool** (parent-signed `child←parent` certs). A receiver accepts a message iff the sender's signing key **resolves to an admitted anchor through a valid cert chain** (`trustGraph.resolve`, the new `lookupPub`). A joiner posts keys to a reserved **waiting-room channel**; the admin SAS-verifies (human; mocked in tests) and admits — adding the anchor + **wrapping the workspace key** (`nacl/box` sealed box) to the joiner's X25519 key. Membership changes **roll a new key-epoch** wrapped to the current anchors.

**Tech stack:** Go; `fabric/e2e` (XChaCha20-Poly1305 + Ed25519 + HKDF, merged) + **add** `golang.org/x/crypto/nacl/box` (X25519 sealed box) + Ed25519 cert primitives; `hubclient.Fake` for two-daemon tests.

## Global Constraints

- **Build on the merged single-host core; non-e2e and single-host e2e stay green.** The full module (`go test ./... -race`) passes at every commit. Generalize in place — no parallel implementations, no compat shims (repo CLAUDE.md "No Legacy / Compat Code"): when the device set becomes the anchor set + cert pool, update all callers in the same commit.
- **No new third-party deps beyond `golang.org/x/crypto`** (already present; `nacl/box` + `curve25519` are sub-packages of it). Stdlib otherwise.
- **Crypto formats are fixed (below); do not improvise wire/signature layouts.** All length-prefixes are uint32-LE; all domain tags are fixed ASCII + `\x00`. Mirror the existing `fabric/e2e` canonicalization style.
- **Relay stays blind.** Message bodies, wrapped keys, and certs that ride channels must be opaque/sealed on the wire; the only necessarily-cleartext payload is a join request (pre-key). Tests assert "relay sees only ciphertext" for key material.
- **The one human step is SAS confirmation at admit.** Never block on it in code or tests — `admit` takes an already-confirmed decision; tests drive it directly.
- `git commit` standalone (never chained after `git add`); `gofmt -w` + `go vet ./...` before every commit; `trash` not `rm`; never log/print key bytes; preserve `state.json` 0600.

## Crypto & data formats (locked)

**X25519 encryption keys.** Every e2e agent gets, alongside its Ed25519 `SignSeed`, an X25519 keypair via `box.GenerateKey(rand.Reader)` → store the 32-byte private as `Agent.EncPriv`. Public is derived: `curve25519.X25519(encPriv, curve25519.Basepoint)`.

**Sealed key wrap.** Wrap the 32-byte workspace key to an anchor's X25519 **public** key with `box.SealAnonymous(nil, key, &anchorPub, rand.Reader)`; unwrap with `box.OpenAnonymous(nil, blob, &anchorPub, &anchorPriv)`. (Sealed box = anonymous sender; perfect for "admin wraps key to joiner".)

**Cert (parent-signs-child).** `type Cert struct { ChildID, ParentID string; ChildSignPub []byte; Sig []byte }` (json-tagged). Signed bytes = `"botbus-e2e-cert-v1\x00" ‖ lp(ChildID) ‖ lp(ParentID) ‖ lp(ChildSignPub)` where `lp` = uint32-LE-length-prefixed. `Sig = ed25519.Sign(parentSignPriv, signedCertPayload)`. Live in `fabric/e2e` (pure, TDD'd): `SignCert(parentPriv, childID, parentID, childSignPub) Cert` and `VerifyCert(c Cert, parentPub ed25519.PublicKey) bool`.

**Admitted-anchor set** = the existing `signedDeviceSet` blob (admin-signed via `applySigned`), reused verbatim: it maps `anchorID → anchorSignPub`. Semantics change (anchors, not flat devices); the wire/verify code does not.

**Cert pool** = parent-signed certs distributed on the roster channel (workspace-key-encrypted). Map `childID → Cert`.

**trustGraph.resolve(id) (ed25519.PublicKey, bool):** if `id` is in the admitted-anchor set → its pub, true. Else look up `cert := pool[id]`; recursively `resolve(cert.ParentID)`; if the parent resolves to `parentPub` **and** `VerifyCert(cert, parentPub)` → return `cert.ChildSignPub, true`. Cycle/length-bounded (cap depth by pool size). This replaces `deviceSet.lookup` as the opener's `lookupPub`.

**Waiting room.** `Workspace.WaitingRoom string` — a hub channel id minted at `workspace create --e2e`, shared out-of-band as the join handle (the `…/join` URL resolves to it; web page deferred). Cleartext join requests are posted here.

**JoinRequest** (cleartext on the waiting room): `type JoinRequest struct { ReqID, Name, ParentIntent string; SignPub, EncPub []byte }`.

**AdmitGrant** (the admit response the joiner reads): `type AdmitGrant struct { ReqID, AnchorID string; Epoch uint32; WrappedKey []byte; RootID string; AdminPub []byte }` — `WrappedKey` is the sealed-box-wrapped workspace key; posted to the waiting room (or a per-request reply channel). The joiner unwraps with its `EncPriv`.

---

## PHASE 1 — Identity & cert chains (cross-host acceptance)

### Task 1: X25519 keys on agents

**Files:** Modify `fabric/agentstate/agentstate.go` (add `Agent.EncPriv []byte` json `encPriv,omitempty`); Modify `fabric/hostagent/hostagent.go` (`newEncKey()` + generate in `Create` when `o.E2E`); Test `fabric/hostagent/hostagent_test.go`.

**Interfaces — Produces:** `Agent.EncPriv []byte` (32-byte X25519 private, present iff e2e); `hostagent` generates it next to `SignSeed`.

- [ ] **Step 1: failing test** — `Create(CreateOpts{E2E:true,...})` yields `len(agent.EncPriv)==32`; `E2E:false` → nil.
```go
func TestCreateGeneratesEncKeyForE2E(t *testing.T) { /* mirror the existing SignSeed test harness; assert len(a.EncPriv)==32 for E2E and nil otherwise */ }
```
- [ ] **Step 2:** run → FAIL (no `EncPriv`).
- [ ] **Step 3:** add field + `func newEncKey() ([]byte, error)` using `box.GenerateKey(rand.Reader)` returning `priv[:]`; in `Create`, inside the existing `if o.E2E {` block, set `a.EncPriv, err = newEncKey()`. Import `golang.org/x/crypto/nacl/box`.
- [ ] **Step 4:** run → PASS; `go test ./fabric/hostagent/ ./fabric/agentstate/`.
- [ ] **Step 5:** commit `feat(e2e): X25519 encryption keys on e2e agents`.

### Task 2: Cert primitives in `fabric/e2e`

**Files:** Create `fabric/e2e/cert.go`; Test `fabric/e2e/cert_test.go`.

**Interfaces — Produces:** `type Cert struct{ChildID,ParentID string; ChildSignPub,Sig []byte}` (json tags `child`,`parent`,`childPub`,`sig`); `func SignCert(parentPriv ed25519.PrivateKey, childID, parentID string, childSignPub ed25519.PublicKey) Cert`; `func VerifyCert(c Cert, parentPub ed25519.PublicKey) bool`; unexported `signedCertPayload(childID, parentID string, childSignPub []byte) []byte` with the locked layout.

- [ ] **Step 1: failing tests** — round-trip (sign→verify true); wrong parent pub → false; tampered `ChildSignPub`/`ChildID` → false; cross-protocol: a sig made over a different domain must not verify (domain-tag separation).
```go
func TestSignVerifyCert(t *testing.T){ /* GenerateKey parent; SignCert; VerifyCert true */ }
func TestVerifyCertRejectsTamper(t *testing.T){ /* flip ChildID -> false; wrong parentPub -> false */ }
```
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3:** implement using the locked `signedCertPayload` layout + `putLenPrefixedBytes` style (replicate the helper locally in e2e if unexported ones aren't reachable from a new file in the same package — they ARE same-package, reuse them).
- [ ] **Step 4:** run → PASS; `go test ./fabric/e2e/ -race`.
- [ ] **Step 5:** commit `feat(e2e): parent-signs-child cert primitives`.

### Task 3: trustGraph (anchor set + cert pool → resolve)

**Files:** Create `fabric/daemon/trustgraph.go`; Test `fabric/daemon/trustgraph_test.go`.

**Interfaces — Consumes:** `deviceSet` (as the admitted-anchor set), `e2e.Cert`, `e2e.VerifyCert`. **Produces:**
- `type trustGraph struct { anchors *deviceSet; mu sync.RWMutex; certs map[string]e2e.Cert }`
- `func newTrustGraph() *trustGraph`
- `func (g *trustGraph) addCert(c e2e.Cert)` (store by `c.ChildID`)
- `func (g *trustGraph) resolve(id string) (ed25519.PublicKey, bool)` — anchor → pub; else cert→resolve(parent)→VerifyCert; **cycle/depth-bounded** (track visited; cap by len(certs)).
- `func (g *trustGraph) applyAnchorSet(blob, sig []byte, adminPub ed25519.PublicKey) error` (delegates to the embedded anchor `deviceSet.applySigned`).

- [ ] **Step 1: failing tests** —
  - direct anchor resolves to its pub;
  - child under an anchor (one cert) resolves to childPub;
  - grandchild (two certs) resolves;
  - an id whose chain never reaches an anchor → `(nil,false)`;
  - a cert with a bad signature → not resolved;
  - a cycle (A.parent=B, B.parent=A), neither an anchor → terminates, `(nil,false)`.
```go
func TestTrustGraphResolvesChainToAnchor(t *testing.T){/* anchorSet has root; certs: mid<-root, leaf<-mid; resolve(leaf)==leafPub */}
func TestTrustGraphRejectsUnanchored(t *testing.T){/* leaf<-mid<-orphan; resolve(leaf) false */}
func TestTrustGraphCycleSafe(t *testing.T){/* A<-B,B<-A; resolve(A) terminates false */}
```
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3:** implement. `resolve` recursion with a `visited map[string]bool` guard; anchors via `g.anchors.lookup`.
- [ ] **Step 4:** run → PASS (`-race`).
- [ ] **Step 5:** commit `feat(e2e): trust graph (admitted anchors + cert chains)`.

### Task 4: Wire trustGraph into the daemon + opener acceptance

**Files:** Modify `fabric/daemon/daemon.go` (replace `devices *deviceSet` with `trust *trustGraph`; init in `NewRuntime`; keep a helper if other code referenced `d.devices`); Modify `fabric/daemon/inbox.go` (`openerFor` uses `d.trust.resolve` as `lookupPub`); Modify `fabric/daemon/daemon.go` `seedDeviceFor` → seed the **anchor set** for local roots AND add a self-cert for non-root local agents; update all `d.devices` references + tests.

**Interfaces — Consumes:** Task 3. **Produces:** opener accepts iff `e2e.OpenMessage(..., d.trust.resolve)` succeeds (sender resolves to an admitted anchor). Same-host agents keep working: a local agent that is its workspace root is seeded as an anchor; local non-root agents get a parent-signed cert added to `d.trust` at attach.

- [ ] **Step 1: failing test** — extend/port the integration tests: a message from a remote agent whose cert chains to an admitted anchor is accepted; an agent with no chain is dropped. (Reuse the two-daemon harness; build certs with `e2e.SignCert`.)
- [ ] **Step 2:** run → FAIL (no `d.trust`).
- [ ] **Step 3:** replace the field; `openerFor`'s e2e branch calls `e2e.OpenMessage(ec.key, ec.channelID, env, d.trust.resolve)`; replay unchanged. Update `seedDeviceFor` (rename `seedLocalTrust`): if the agent is its workspace root → `d.trust.anchors.set(id,pub)` (local anchor); else add a cert `SignCert(parentPriv, child, parent, childPub)` to `d.trust` (the parent's SignSeed is in local state). Update every `d.devices.*` caller + the struct-literal Daemons in tests (`devices:` → `trust:` with `newTrustGraph()`).
- [ ] **Step 4:** run full pkg `-race` → PASS.
- [ ] **Step 5:** commit `feat(e2e): opener accepts via cert-chain-to-anchor (trust graph)`.

### Task 5: Cert distribution on the roster channel

**Files:** Modify `fabric/daemon/inbox.go` / wherever roster frames are ingested (extend the device-set/roster ingest to also ingest certs); add `func (d *Daemon) publishCert(ctx, c e2e.Cert) error` (encrypt under workspace key + publish to the roster channel); call it at attach for non-root e2e agents. Test in `fabric/daemon/`.

**Interfaces — Produces:** certs ride the roster channel encrypted; a receiving daemon ingests them into `d.trust.addCert` after decrypting. Reuse the e2e seal path (a cert is just bytes sealed under the workspace key; it needs no per-device signature beyond the parent sig already inside the cert — but it MUST be confidential, so seal it).

- [ ] **Step 1: failing test** — daemon A publishes a cert for its non-root agent; daemon B (same workspace key) ingests it and `dB.trust.resolve(childID)` succeeds once B also has the anchor set.
- [ ] **Step 2/3/4:** implement seal-publish + ingest-decrypt-addCert; run `-race`.
- [ ] **Step 5:** commit `feat(e2e): distribute parent-signed certs over the roster channel`.

---

## PHASE 2 — Waiting room & admission

### Task 6: Waiting-room channel minted at `workspace create --e2e`

**Files:** Modify `fabric/agentstate/agentstate.go` (`Workspace.WaitingRoom string` json `waitingRoom,omitempty`); Modify `cmd/botbus/workspace.go` (mint a channel via `hub.MintChannel` at create --e2e, store it; print the join handle). Test the create path helper.

- [ ] TDD steps: failing test that create --e2e populates `Workspace.WaitingRoom`; implement; pass; commit `feat(e2e): mint waiting-room channel on workspace create --e2e`.

### Task 7: Join-request + AdmitGrant codec in `fabric/e2e` (or daemon)

**Files:** Create `fabric/daemon/admission.go` (the `JoinRequest`/`AdmitGrant` structs + JSON encode/decode + sealed-key wrap/unwrap helpers using `nacl/box`); Test.

**Interfaces — Produces:** `wrapKey(workspaceKey [32]byte, anchorEncPub [32]byte) ([]byte, error)` (SealAnonymous); `unwrapKey(blob []byte, encPriv [32]byte) ([32]byte, bool)` (OpenAnonymous → derive pub from priv); `JoinRequest`/`AdmitGrant` marshal/unmarshal.

- [ ] TDD: wrap→unwrap round-trips the key; wrong priv fails; structs round-trip JSON. Commit `feat(e2e): admission codec + sealed-box key wrap`.

### Task 8: `workspace join` (joiner side)

**Files:** Modify `cmd/botbus/workspace.go` (new `join` subcommand: takes the waiting-room handle + a name; generates Sign+Enc keys; posts a `JoinRequest` to the waiting room; polls for an `AdmitGrant` matching its `ReqID`; on receipt unwraps the key and writes a local `Workspace{E2E,RootID,Epoch,Key,WaitingRoom,AdminPub}` + a self `Agent` with the seeds). Test the request/grant handling against a fake hub.

- [ ] TDD: drive a fake hub — post request, inject a matching grant, assert the joiner stores the unwrapped key + workspace. Commit `feat(e2e): workspace join (post request, unwrap granted key)`.

### Task 9: `workspace pending` + `workspace admit` (admin side)

**Files:** Modify `cmd/botbus/workspace.go` (`pending`: read+list join requests with a **SAS fingerprint** of each request's keys; `admit <reqID>`: takes the human's already-made decision, adds the anchor to the admitted-anchor set + re-signs with `AdminPriv` + publishes; wraps the current key to the request's `EncPub` and posts an `AdmitGrant`). Add `func sasFingerprint(signPub, encPub []byte) string` (short, e.g. 6-group base32 of a hash). Test `admit` end-to-end on a fake hub (SAS shown, decision injected).

- [ ] TDD: `pending` lists an injected request with a stable SAS string; `admit` publishes an updated anchor set (verifiable with AdminPub) + an AdmitGrant whose WrappedKey the requester's EncPriv unwraps. Commit `feat(e2e): workspace pending + admit (SAS, anchor add, key wrap)`.

### Task 10: Two-host join integration test

**Files:** Create `fabric/daemon/cross_host_integration_test.go`.

- [ ] One fake hub; admin daemon (has workspace key + AdminPriv) and a joiner. Joiner posts request → admin admits (SAS decision injected) → joiner unwraps key → joiner sends a message → admin's opener accepts it (cert chain: joiner is an admitted anchor). Assert: (a) the waiting room's AdmitGrant frame carries only a **sealed** WrappedKey (no plaintext key bytes); (b) convergence (admin reads joiner's message); (c) a non-admitted joiner's message is dropped. Commit `test(e2e): two-host waiting-room join + convergence`.

---

## PHASE 3 — Rotation & removal

### Task 11: `key rotate` (epoch roll, re-wrap to current anchors)

**Files:** Modify `cmd/botbus/workspace.go` (or a new `key` subcommand): generate a fresh 32-byte key, bump `Workspace.Epoch`, re-wrap to every current anchor's EncPub (need anchors' EncPubs — see note), publish new AdmitGrants/wrapped blobs + the epoch bump in the anchor set. Keep the prior key for the retention window so in-flight old-epoch frames still open. Test.

> Note: admit must also record each admitted anchor's **EncPub** (extend the anchor-set entry or a side map persisted in `Workspace`) so rotation can re-wrap. Fold that into Task 9's stored state if cleaner; if you change the anchor-set entry shape, update Task 3/9 in the same commit.

- [ ] TDD: rotate → new epoch; a current anchor unwraps the new key; messages seal/open under the new epoch; an old-epoch frame still opens within retention. Commit `feat(e2e): key rotate (epoch roll + re-wrap to anchors)`.

### Task 12: `workspace remove <anchor>` (evict subtree)

**Files:** Modify `cmd/botbus/workspace.go`: drop the anchor from the admitted-anchor set (re-sign + publish) **and** rotate (Task 11) so the removed anchor never gets the new epoch key. Test: removed anchor's subsequent messages are dropped (no chain to a current anchor) and it cannot unwrap the new epoch key.

- [ ] TDD + commit `feat(e2e): workspace remove (drop anchor + rotate to evict)`.

### Task 13: README + limitations

**Files:** Modify the relevant README: document `workspace create --e2e` → share the join handle; `join` / `pending` / `admit` / `key rotate` / `remove`; restate the spec's honest limitations (accept-vs-read same-host; per-epoch FS; metadata cleartext; admin trust root). Commit `docs(e2e): cross-host admission usage + limitations`.

---

## Self-Review

**Spec coverage (vs `botbus/docs/specs/2026-06-26-e2e-cross-host-admission-design.md`):** per-node admission + subtree trust = Tasks 2–4; cert distribution = Task 5; waiting room + SAS + admit = Tasks 6–9; wrap-to-anchor key dist = Tasks 7,9; rotate-on-change = Task 11; removal/evict = Task 12; two-host convergence + relay-blind self-test = Tasks 4,5,10,11,12. Deferred (noted in spec phasing): web `/join` page (Phase-2 browser joiner — daemon path covers the protocol), hub capability tokens (Phase-4).

**Placeholder scan:** crypto/wire formats are locked in the "Crypto & data formats" section; CLI-glue tasks reference exact structs/funcs from earlier tasks. The few "mirror the existing harness" notes point at concrete existing tests to copy.

**Type consistency:** `Cert`, `trustGraph.resolve` (the `lookupPub` shape `func(string)(ed25519.PublicKey,bool)` matches `e2e.OpenMessage`), `wrapKey`/`unwrapKey`, `JoinRequest`/`AdmitGrant`, `Workspace.WaitingRoom`/`EncPriv` are defined before use and consumed unchanged downstream.

**Scope:** one coherent cross-host feature in three phases. Phase fault lines (1 = trust/acceptance, 2 = admission, 3 = lifecycle) are natural smaller-PR boundaries if needed.
