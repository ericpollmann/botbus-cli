# E2E Cross-Host Runnable Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the already-merged cross-host e2e protocol actually runnable: CLI subcommands (`workspace join`/`pending`/`admit`/`key-rotate`/`remove`) plus the runtime roster + waiting-room subscribe-loops that drive `AdmitJoinRequest`/`ProcessAdmitGrant`/`RotateKey`/`RemoveAnchor`, with key rotation that takes effect live.

**Architecture:** botbus-cli has **no daemon IPC** — every CLI invocation is a one-shot process that loads `state.json`, constructs a `*daemon.Daemon`, performs work, and saves `state.json`. The long-running `botbus daemon` process runs the subscribe-loops (`runInbox` per agent today; this plan adds `runRoster` + `runWaitingRoom` per e2e workspace). The two sides stay coherent **through the encrypted roster channel**: anchor-set changes and rekeys propagate as roster frames that every host's `runRoster` loop ingests. State that one-shot rotation/removal needs (the admitted-anchor records, including each anchor's X25519 enc-pubkey) is therefore **persisted to the workspace** so a fresh process rebuilds a complete anchor set.

**Tech Stack:** Go 1.x, stdlib `crypto/ed25519`, `golang.org/x/crypto/{curve25519,nacl/box}`, stdlib `flag` (manual subcommand dispatch), stdlib `testing` + `hubclient.Fake` (in-process fake relay). No new dependencies.

## Global Constraints

- **No new dependencies.** Everything needed is already imported (`x/crypto` is approved).
- **Never print or log key bytes.** Workspace keys, wrapped keys, and private seeds must never appear in logs, stdout, or error strings.
- **`state.json` is mode 0600**, written atomically via `agentstate.Save`. Never lower the mode; never write secrets to any other path.
- **No legacy/compat shims.** When a field or method is replaced (e.g. `d.anchorEnc` → `Workspace.Anchors`), delete the old one and update every caller + test in the same commit.
- **Relay-blind invariant.** The relay (hub) must only ever carry ciphertext for message bodies *and* wrapped key blobs. Any new frame published to a channel must keep plaintext keys out of the wire. Tests assert this with `fake.Published(channel)` substring checks.
- **Admit does NOT rotate the key** (maintainer decision 2026-06-27). Only `remove` rotates (via `RemoveAnchor` → `RotateKey`), and the explicit `key-rotate` command.
- **TDD, frequent commits.** Each task: failing test → run-it-fails → minimal impl → run-it-passes → commit. Run `git commit` as its own standalone Bash call (never chained after `git add`).
- **Branch:** `feat/e2e-runnable` (worktree `.claude/worktrees/runnable`). Draft-first PR; mark ready only when `go build ./...` and `go test ./...` are green.
- **Baseline is green:** `go build ./...`, `go test ./fabric/daemon/... ./fabric/agentstate/...` pass at the start of this plan. Keep them green at every commit.

## Repository map (paths relative to the worktree root)

| Path | Role |
|---|---|
| `fabric/agentstate/agentstate.go` | `State`/`Agent`/`Workspace` structs; `Load`/`Save`/`SetCursor`; `AgentByID`/`WorkspaceFor`/`WorkspaceRootID` |
| `fabric/daemon/daemon.go` | `Daemon` struct + `NewRuntime`/`New`; `attach`, `seedLocalTrust`, `reloadFromDisk`, `RunOn` (long-running loop) |
| `fabric/daemon/admission.go` | `JoinRequest`, `AdmitGrant`, `wrapKey`/`unwrapKey`, `AdmitJoinRequest`, `ProcessAdmitGrant`, `grantSignedPayload`, `sasFingerprint`, `parseJoinRequest`/`parseAdmitGrant` |
| `fabric/daemon/lifecycle.go` | `RotateKey`, `RemoveAnchor` |
| `fabric/daemon/roster.go` | `rosterFrame`, `sealRosterFrame`/`openRosterFrame`, `publishCert`, `ingestRosterFrame`, `rosterAAD` |
| `fabric/daemon/inbox.go` | `opener` type, `openerFor`, `runInbox`, `unwrap`, `sleepCtx`, `reconnectBackoff` |
| `fabric/daemon/e2ectx.go` | `e2eCtx`, `e2eContextFor`, `keyArray`, `nextCounter` |
| `fabric/daemon/trustgraph.go` | `trustGraph` (`anchors *deviceSet`, `certs`), `resolve`, `addCert`, `applyAnchorSet` |
| `fabric/daemon/deviceset.go` | `deviceSet`, `signedDeviceSet`, `marshalDeviceSet`, `applySigned` |
| `cmd/botbus/workspace.go` | `workspaceCmd` dispatcher + `workspaceCreate`/`workspaceInvite`/`workspaceUse` |
| `cmd/botbus/agent.go` | `realDeps()` → `hostagent.Deps{Hub, Control, StatePath, MintKey}` |
| `fabric/daemon/*_test.go` | `hubclient.NewFake()` patterns; `cross_host_admission_test.go`, `e2e_integration_test.go`, `lifecycle_test.go` |

## Key facts the implementer must hold

- **`d.state` is `*agentstate.State`** (a pointer); `WorkspaceFor` returns `&d.state.Workspaces[i]` (a live pointer into the slice). Mutating `ws.Key`/`ws.Epoch` through that pointer is visible to the opener — **but** concurrent read/write of `ws.Key` is a data race, so all rotation-time writes and opener-time reads of `ws.Key` go through `d.mu`.
- **On the receive/open path, the only key-epoch-sensitive value is `ec.key`.** `openerFor`'s replay check already uses `env.KeyEpoch` (from the frame), not `ec.epoch`. So the I-2 fix is solely: re-read `ws.Key` per frame. `channelID` (= `ws.RootID`), `deviceID`, and `devPriv` are rotation-invariant and may stay captured.
- **Roster frame discriminator:** cert/anchors frames are `base64(e2e.Envelope.Marshal())` — base64 alphabet never starts with `{`. Rekey frames (this plan) are JSON `AdmitGrant` — always start with `{`. So `b64[0] == '{'` ⇒ rekey grant; else ⇒ sealed cert/anchors frame.
- **`hubclient.Fake`:** `Publish(ctx, ch, data)`, `Published(ch) []string`, `Inject(ch, frame)`, `Subscribe(ctx, ch, cursor) (<-chan Frame, error)`. **`Subscribe` delivers only frames injected/published *after* subscribe returns** (no history replay). Loop tests must subscribe (or start the loop) *then* `Inject`/`Publish`.
- **`AdmitGrant.AnchorID` == the joiner's `JoinRequest.ReqID`.** This plan makes `join` set `ReqID` = the new local agent's ID, so `AnchorID` is always a local agent ID on the recipient, and the recipient finds its unwrap key via `state.AgentByID(grant.AnchorID).EncPriv`.

---

### Task 1: Persist admitted-anchor records on the Workspace

Replace the in-memory-only `d.anchorEnc` map (lost on restart / absent in a fresh one-shot process — finding I-3) with a persisted `Workspace.Anchors` list, and add a trust-graph hydration helper so any process rebuilds a complete anchor set.

**Files:**
- Modify: `fabric/agentstate/agentstate.go` (add `AnchorRef` + `Workspace.Anchors`)
- Modify: `fabric/daemon/daemon.go` (delete `anchorEnc` field; add `hydrateWorkspaceTrust`)
- Modify: `fabric/daemon/admission.go` (`AdmitJoinRequest`: append to `ws.Anchors` instead of `d.anchorEnc`)
- Modify: `fabric/daemon/lifecycle.go` (`RotateKey`: re-wrap from `ws.Anchors`; `RemoveAnchor`: drop from `ws.Anchors`)
- Test: `fabric/daemon/anchors_persist_test.go` (new)
- Modify (if they reference `d.anchorEnc`): `fabric/daemon/lifecycle_test.go`

**Interfaces:**
- Produces:
  - `agentstate.AnchorRef{ID string; SignPub []byte; EncPub []byte}` (json `id`/`signPub`/`encPub`)
  - `Workspace.Anchors []AnchorRef` (json `anchors,omitempty`) — the admin host's record of admitted **remote** anchors (the local root is *not* listed; it is seeded from its agent's `SignSeed`).
  - `func (d *Daemon) hydrateWorkspaceTrust(ws *agentstate.Workspace)` — idempotent: seeds local agents (via existing `seedLocalTrust`) **and** sets every `ws.Anchors` record into `d.trust.anchors`. Call before any one-shot admit/rotate/remove and at daemon startup.
- Consumes: existing `d.trust.anchors.set/snapshot/remove`, `seedLocalTrust`, `marshalDeviceSet`.

- [ ] **Step 1: Write the failing test** — `fabric/daemon/anchors_persist_test.go`. Reuse the `newAdminDaemon`/`admitAnchor` helpers in `lifecycle_test.go`.

```go
package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"golang.org/x/crypto/nacl/box"
)

// After AdmitJoinRequest, the joiner must be recorded in ws.Anchors (persisted),
// and a FRESH Daemon (no in-memory anchorEnc) reconstructed from that state must
// still re-wrap a rotation to the joiner.
func TestAdmitPersistsAnchorAndFreshRotateRewraps(t *testing.T) {
	d, fake, ws := newAdminDaemon(t)
	ctx := context.Background()

	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	encPub, encPriv, _ := box.GenerateKey(rand.Reader)
	req := JoinRequest{ReqID: "joiner-1", Name: "joiner", SignPub: signPub, EncPub: encPub[:]}
	if _, err := d.AdmitJoinRequest(ctx, ws, req); err != nil {
		t.Fatalf("AdmitJoinRequest: %v", err)
	}

	// Persisted on the workspace.
	if len(ws.Anchors) != 1 || ws.Anchors[0].ID != "joiner-1" {
		t.Fatalf("ws.Anchors not persisted: %+v", ws.Anchors)
	}

	// Simulate a process restart: a brand-new Daemon over the SAME state, with an
	// empty trust graph, hydrated only from persisted state.
	fresh := &Daemon{state: d.state, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	fresh.hydrateWorkspaceTrust(ws)

	rosterBefore := len(fake.Published("roster"))
	if _, err := fresh.RotateKey(ctx, ws); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	// RotateKey must have published a rekey targeting joiner-1, decryptable by encPriv.
	found := false
	for _, f := range fake.Published("roster")[rosterBefore:] {
		if len(f) == 0 || f[0] != '{' {
			continue // sealed anchors frame, not a rekey grant
		}
		g, err := parseAdmitGrant([]byte(f))
		if err != nil || g.AnchorID != "joiner-1" {
			continue
		}
		if _, ok := unwrapKey(g.WrappedKey, *encPriv); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("fresh RotateKey did not re-wrap to the persisted anchor")
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./fabric/daemon/ -run TestAdmitPersistsAnchorAndFreshRotateRewraps -v`. Expected: compile error (`hydrateWorkspaceTrust` undefined, `ws.Anchors` undefined) or assertion failure.

- [ ] **Step 3: Add the model.** In `fabric/agentstate/agentstate.go`, add above `Workspace`:

```go
// AnchorRef is a persisted record of an admitted remote anchor: the identity
// (Ed25519) the admin signs into the anchor set, and the X25519 encryption
// pubkey a rotated workspace key is wrapped to. Stored on the admin host only.
type AnchorRef struct {
	ID      string `json:"id"`
	SignPub []byte `json:"signPub"`
	EncPub  []byte `json:"encPub"`
}
```

Add to `Workspace` (after `WaitingRoom`):

```go
	Anchors []AnchorRef `json:"anchors,omitempty"` // admitted remote anchors (admin host); source of truth for rekey re-wraps
```

- [ ] **Step 4: Add hydration + swap `d.anchorEnc` → `ws.Anchors`.**

In `fabric/daemon/daemon.go`: **delete** the `anchorEnc map[string][]byte` field and the `anchorEnc: make(map[string][]byte),` line in `NewRuntime`. Remove `anchorEnc` from the `mu` doc comment. Add:

```go
// hydrateWorkspaceTrust rebuilds the in-memory trust graph for ws from persisted
// state so a fresh process (one-shot CLI or restarted daemon) has the COMPLETE
// anchor set: local agents (root → anchor, children → parent-signed certs) plus
// every persisted remote anchor. Idempotent.
func (d *Daemon) hydrateWorkspaceTrust(ws *agentstate.Workspace) {
	for _, a := range d.state.Agents {
		if d.state.WorkspaceRootID(a.ID) == ws.RootID {
			d.seedLocalTrust(a)
		}
	}
	for _, ar := range ws.Anchors {
		if len(ar.SignPub) == ed25519.SeedSize || len(ar.SignPub) == ed25519.PublicKeySize {
			d.trust.anchors.set(ar.ID, ed25519.PublicKey(ar.SignPub))
		}
	}
}
```

In `admission.go` `AdmitJoinRequest`, replace the `d.anchorEnc` block (lines ~135-146) with a persisted append (keep the local `d.trust.applyAnchorSet` apply above it unchanged):

```go
	// Record the joiner as a persisted anchor (source of truth for future rekeys).
	if len(req.EncPub) == 32 {
		ws.Anchors = upsertAnchor(ws.Anchors, agentstate.AnchorRef{
			ID:      req.ReqID,
			SignPub: append([]byte(nil), req.SignPub...),
			EncPub:  append([]byte(nil), req.EncPub...),
		})
	}
```

Add the helper (in `admission.go` or `lifecycle.go`):

```go
func upsertAnchor(list []agentstate.AnchorRef, a agentstate.AnchorRef) []agentstate.AnchorRef {
	for i := range list {
		if list[i].ID == a.ID {
			list[i] = a
			return list
		}
	}
	return append(list, a)
}
```

In `lifecycle.go` `RotateKey`, replace the `d.anchorEnc` snapshot block (lines ~50-57) and the loop's source so it iterates `ws.Anchors`:

```go
	// 4. Re-wrap the new key to each persisted anchor.
	for _, ar := range ws.Anchors {
		if len(ar.EncPub) != 32 {
			continue
		}
		var encPub [32]byte
		copy(encPub[:], ar.EncPub)
		wrapped, err := wrapKey(newKey, encPub)
		if err != nil {
			return [32]byte{}, err
		}
		// (rekey publish — Task 2 changes the wire format; keep current sealed form for now)
		rekeySealed, err := sealRosterFrame(newKey, newEpoch, rosterFrame{Kind: "rekey", WrappedKey: wrapped, AnchorID: ar.ID})
		if err != nil {
			return [32]byte{}, err
		}
		if err := d.hub.Publish(ctx, ws.Roster, rekeySealed); err != nil {
			return [32]byte{}, err
		}
	}
```

In `lifecycle.go` `RemoveAnchor`, replace the `delete(d.anchorEnc, anchorID)` block with `ws.Anchors` removal:

```go
func (d *Daemon) RemoveAnchor(ctx context.Context, ws *agentstate.Workspace, anchorID string) ([32]byte, error) {
	d.trust.anchors.remove(anchorID)
	out := ws.Anchors[:0]
	for _, ar := range ws.Anchors {
		if ar.ID != anchorID {
			out = append(out, ar)
		}
	}
	ws.Anchors = out
	return d.RotateKey(ctx, ws)
}
```

Update `lifecycle.go`'s `RotateKey` doc comment: remove the stale `anchorEnc … not persisted` paragraph; state that rekeys are re-wrapped to `ws.Anchors`.

- [ ] **Step 5: Fix any test referencing `d.anchorEnc`.** Grep: `rg 'anchorEnc' fabric/`. Update/remove references (the `admitAnchor` helper in `lifecycle_test.go` no longer needs to return the enc keys via `d.anchorEnc`; anchors now live on `ws.Anchors`).

- [ ] **Step 6: Run tests** — `go test ./fabric/daemon/ ./fabric/agentstate/ -v`. Expected: PASS (including the new test).

- [ ] **Step 7: Commit**

```bash
git add fabric/agentstate/agentstate.go fabric/daemon/daemon.go fabric/daemon/admission.go fabric/daemon/lifecycle.go fabric/daemon/anchors_persist_test.go fabric/daemon/lifecycle_test.go
git commit -m "feat(e2e): persist admitted anchors on the workspace (drop in-memory anchorEnc)"
```

---

### Task 2: Authenticated rekey delivery a recipient can actually open

The merged `RotateKey` seals each rekey frame **under the new key** (`sealRosterFrame(newKey, …)`), which the recipient does not have yet — so no joiner can adopt a rotation. Fix: deliver each rekey as an admin-signed `AdmitGrant`-shaped message published **cleartext** to the roster (the `WrappedKey` is sealed-box ciphertext, so the relay still sees only ciphertext key material). Publish rekeys **before** the new anchors frame so a sequential ingester adopts the new key, then opens the (newKey-sealed) anchors frame.

**Files:**
- Modify: `fabric/daemon/admission.go` (factor out `verifyGrant`; add `ProcessRekey`)
- Modify: `fabric/daemon/lifecycle.go` (`RotateKey`: emit signed rekey grants; reorder)
- Modify: `fabric/daemon/roster.go` (`rosterFrame`: drop the now-unused `rekey` Kind / `WrappedKey` / `AnchorID` fields)
- Test: `fabric/daemon/rekey_delivery_test.go` (new)

**Interfaces:**
- Produces:
  - `func verifyGrant(grant AdmitGrant, encPriv []byte, expectedAdminPub []byte) ([32]byte, bool)` — shared core: validates `expectedAdminPub`, the grant signature, and unwraps the key. Returns `(key, ok)`.
  - `func ProcessRekey(grant AdmitGrant, encPriv []byte, expectedAdminPub []byte) (key [32]byte, epoch uint32, ok bool)` — for an existing member adopting a rotated key.
  - `RotateKey` now publishes, per anchor, `grant.Marshal()` (JSON, starts with `{`) to `ws.Roster`, then the sealed anchors frame.
- Consumes: `grantSignedPayload`, `unwrapKey`, `wrapKey`, `ws.Anchors` (Task 1).

- [ ] **Step 1: Write the failing test** — `fabric/daemon/rekey_delivery_test.go`:

```go
package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestRotatePublishesOpenableSignedRekey(t *testing.T) {
	d, fake, ws := newAdminDaemon(t)
	ctx := context.Background()
	adminPub := append([]byte(nil), ws.AdminPub...)

	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	encPub, encPriv, _ := box.GenerateKey(rand.Reader)
	if _, err := d.AdmitJoinRequest(ctx, ws, JoinRequest{ReqID: "j1", SignPub: signPub, EncPub: encPub[:]}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	before := len(fake.Published("roster"))
	newKey, err := d.RotateKey(ctx, ws)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	var adopted bool
	for _, f := range fake.Published("roster")[before:] {
		// Relay-blind: the new key bytes must never appear in the wire frame.
		if strings.Contains(f, string(newKey[:])) {
			t.Fatal("new key bytes leaked into a roster frame")
		}
		if len(f) == 0 || f[0] != '{' {
			continue
		}
		g, perr := parseAdmitGrant([]byte(f))
		if perr != nil || g.AnchorID != "j1" {
			continue
		}
		key, epoch, ok := ProcessRekey(g, encPriv[:], adminPub)
		if !ok {
			t.Fatal("ProcessRekey rejected a valid signed rekey")
		}
		if key != newKey || epoch != ws.Epoch {
			t.Fatalf("rekey mismatch: epoch=%d want %d", epoch, ws.Epoch)
		}
		adopted = true
	}
	if !adopted {
		t.Fatal("no openable signed rekey grant for j1 was published")
	}
}

func TestProcessRekeyRejectsWrongAdmin(t *testing.T) {
	d, _, ws := newAdminDaemon(t)
	ctx := context.Background()
	encPub, encPriv, _ := box.GenerateKey(rand.Reader)
	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, _ = d.AdmitJoinRequest(ctx, ws, JoinRequest{ReqID: "j1", SignPub: signPub, EncPub: encPub[:]})
	g, _ := d.RotateKeyGrantFor(ctx, ws, "j1") // helper below, or reconstruct from Published
	_ = g
	wrongAdmin, _, _ := ed25519.GenerateKey(rand.Reader)
	// A grant verified against the wrong admin pubkey must be rejected.
	if _, _, ok := ProcessRekey(g, encPriv[:], wrongAdmin); ok {
		t.Fatal("ProcessRekey accepted a grant under the wrong admin pubkey")
	}
}
```

> Note: if adding a `RotateKeyGrantFor` test helper is awkward, drop `TestProcessRekeyRejectsWrongAdmin`'s use of it and instead pull the rekey grant for `j1` out of `fake.Published("roster")` exactly as `TestRotatePublishesOpenableSignedRekey` does. The required assertion is only that `ProcessRekey` returns `ok == false` under a wrong admin pubkey.

- [ ] **Step 2: Run test to verify it fails** — `go test ./fabric/daemon/ -run 'TestRotatePublishesOpenableSignedRekey|TestProcessRekeyRejectsWrongAdmin' -v`. Expected: compile error (`ProcessRekey` undefined) / failure (current rekey frames are sealed, not JSON grants).

- [ ] **Step 3: Factor the shared verify core + add `ProcessRekey`.** In `admission.go`, extract from `ProcessAdmitGrant`:

```go
// verifyGrant validates expectedAdminPub, the admin signature, and unwraps the
// workspace key. Returns (key, true) only when all three succeed.
func verifyGrant(grant AdmitGrant, encPriv []byte, expectedAdminPub []byte) ([32]byte, bool) {
	if len(encPriv) != 32 {
		return [32]byte{}, false
	}
	if len(expectedAdminPub) == 0 || !bytes.Equal(grant.AdminPub, expectedAdminPub) {
		return [32]byte{}, false
	}
	if !ed25519.Verify(ed25519.PublicKey(grant.AdminPub), grantSignedPayload(grant), grant.Sig) {
		return [32]byte{}, false
	}
	var priv [32]byte
	copy(priv[:], encPriv)
	return unwrapKey(grant.WrappedKey, priv)
}

// ProcessRekey validates a signed rekey grant published on the roster and
// returns the new workspace key + epoch for an existing member to adopt.
func ProcessRekey(grant AdmitGrant, encPriv []byte, expectedAdminPub []byte) (key [32]byte, epoch uint32, ok bool) {
	k, good := verifyGrant(grant, encPriv, expectedAdminPub)
	if !good {
		return [32]byte{}, 0, false
	}
	return k, grant.Epoch, true
}
```

Rewrite `ProcessAdmitGrant` to call `verifyGrant`:

```go
func ProcessAdmitGrant(grant AdmitGrant, encPriv []byte, expectedAdminPub []byte) (*agentstate.Workspace, [32]byte, bool) {
	key, ok := verifyGrant(grant, encPriv, expectedAdminPub)
	if !ok {
		return nil, [32]byte{}, false
	}
	ws := &agentstate.Workspace{
		RootID: grant.RootID, E2E: true, Epoch: grant.Epoch, Key: key[:],
		AdminPub: grant.AdminPub, Roster: grant.Roster, WaitingRoom: grant.WaitingRoom,
	}
	return ws, key, true
}
```

- [ ] **Step 4: Emit signed rekey grants from `RotateKey`; reorder.** In `lifecycle.go` `RotateKey`, move the per-anchor re-wrap loop **before** the anchors-frame publish, and emit grants instead of sealed frames:

```go
	adminPriv := ed25519.NewKeyFromSeed(ws.AdminPriv)

	// 3. Re-wrap the new key to each persisted anchor as a signed rekey grant
	//    (cleartext on the wire; WrappedKey is sealed-box ciphertext). Published
	//    BEFORE the anchors frame so a sequential ingester adopts the new key
	//    first, then can open the newKey-sealed anchors frame.
	for _, ar := range ws.Anchors {
		if len(ar.EncPub) != 32 {
			continue
		}
		var encPub [32]byte
		copy(encPub[:], ar.EncPub)
		wrapped, err := wrapKey(newKey, encPub)
		if err != nil {
			return [32]byte{}, err
		}
		grant := AdmitGrant{
			ReqID: ar.ID, AnchorID: ar.ID, RootID: ws.RootID, Epoch: newEpoch,
			WrappedKey: wrapped, AdminPub: ws.AdminPub, Roster: ws.Roster, WaitingRoom: ws.WaitingRoom,
		}
		grant.Sig = ed25519.Sign(adminPriv, grantSignedPayload(grant))
		gb, err := grant.Marshal()
		if err != nil {
			return [32]byte{}, err
		}
		if err := d.hub.Publish(ctx, ws.Roster, string(gb)); err != nil {
			return [32]byte{}, err
		}
	}

	// 4. Re-publish the anchor set at the new epoch (sealed under newKey).
	blob := marshalDeviceSet(signedDeviceSet{Epoch: newEpoch, Devices: d.trust.anchors.snapshot()})
	sig := ed25519.Sign(adminPriv, blob)
	if err := d.trust.applyAnchorSet(blob, sig, ed25519.PublicKey(ws.AdminPub)); err != nil {
		return [32]byte{}, err
	}
	anchorsSealed, err := sealRosterFrame(newKey, newEpoch, rosterFrame{Kind: "anchors", AnchorBlob: blob, AnchorSig: sig})
	if err != nil {
		return [32]byte{}, err
	}
	if err := d.hub.Publish(ctx, ws.Roster, anchorsSealed); err != nil {
		return [32]byte{}, err
	}
	return newKey, nil
```

(Delete the old anchors-first block and the old sealed-rekey block from Task 1's interim form.)

- [ ] **Step 5: Drop the dead `rekey` rosterFrame fields.** In `roster.go`, remove `WrappedKey` and `AnchorID` from `rosterFrame` and drop `| "rekey"` from the `Kind` comment (cert/anchors only travel sealed now). Confirm nothing else references them: `rg 'rosterFrame\{Kind: "rekey"' ; rg '\.WrappedKey|\.AnchorID' fabric/daemon/roster.go`.

- [ ] **Step 6: Run tests** — `go test ./fabric/daemon/ -v`. Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add fabric/daemon/admission.go fabric/daemon/lifecycle.go fabric/daemon/roster.go fabric/daemon/rekey_delivery_test.go
git commit -m "feat(e2e): deliver rotations as openable admin-signed rekey grants"
```

---

### Task 3: Opener re-reads the key per frame; roster ingest adopts rekeys

Two coupled changes that make rotation take effect live: (a) `openerFor` reads `ws.Key` per frame under `d.mu` (finding I-2); (b) `ingestRosterFrame` recognizes a JSON rekey grant, adopts the new key under `d.mu`, and persists it.

**Files:**
- Modify: `fabric/daemon/inbox.go` (`openerFor` closure re-reads key)
- Modify: `fabric/daemon/e2ectx.go` (add `currentKey` locked reader; keep `e2eContextFor` for the static fields)
- Modify: `fabric/daemon/roster.go` (`ingestRosterFrame` dispatches grant vs sealed; `applyRekey`)
- Modify: `fabric/daemon/daemon.go` (add `persistWorkspaceKey` helper)
- Test: `fabric/daemon/rotation_live_test.go` (new), run with `-race`

**Interfaces:**
- Produces:
  - `func (d *Daemon) currentKey(ws *agentstate.Workspace) ([32]byte, bool)` — `ws.Key` copied out under `d.mu`.
  - `ingestRosterFrame` now also adopts rekey grants addressed to a local anchor.
  - `func (d *Daemon) persistWorkspaceKey(ws *agentstate.Workspace)` — best-effort save of the workspace key/epoch to `state.json` (no-op when `statePath == ""`, as in tests).
- Consumes: `ProcessRekey` (Task 2), `state.AgentByID(...).EncPriv`, `ws.AdminPub`.

- [ ] **Step 1: Write the failing test** — `fabric/daemon/rotation_live_test.go`. Bob is an admitted anchor; after Bob's roster loop ingests a rekey grant, Bob's opener must decrypt a message Alice (admin) sends under the new key.

```go
package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"golang.org/x/crypto/nacl/box"
)

func TestOpenerAdoptsRotatedKeyLive(t *testing.T) {
	ctx := context.Background()
	// --- Admin (Alice) workspace with one admitted anchor (Bob). ---
	d, fake, ws := newAdminDaemon(t)
	bobSignPub, bobSignSeed, _ := ed25519.GenerateKey(rand.Reader)
	bobEncPub, bobEncPriv, _ := box.GenerateKey(rand.Reader)
	if _, err := d.AdmitJoinRequest(ctx, ws, JoinRequest{ReqID: "bob", SignPub: bobSignPub, EncPub: bobEncPub[:]}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	// --- Bob's daemon: knows the workspace (epoch 1 key) + holds bob's enc/sign keys. ---
	adminPub := append([]byte(nil), ws.AdminPub...)
	bobState := &agentstate.State{
		Agents: []agentstate.Agent{{ID: "bob", SignSeed: bobSignSeed.Seed(), EncPriv: bobEncPriv[:], InboxChannel: "bob-inbox"}},
		Workspaces: []agentstate.Workspace{{
			RootID: "bob", E2E: true, Epoch: ws.Epoch, Key: append([]byte(nil), ws.Key...),
			AdminPub: adminPub, Roster: ws.Roster, WaitingRoom: ws.WaitingRoom,
		}},
	}
	dBob := &Daemon{state: bobState, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	bobWs := &bobState.Workspaces[0]
	// Bob trusts admin's anchor set so it can verify Alice's signatures.
	dBob.hydrateWorkspaceTrust(bobWs)
	dBob.trust.anchors = d.trust.anchors // share the admin-signed anchor set for the test

	// --- Admin rotates; capture the rekey grant aimed at bob and feed Bob's ingest. ---
	before := len(fake.Published("roster"))
	if _, err := d.RotateKey(ctx, ws); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	for _, f := range fake.Published("roster")[before:] {
		dBob.ingestRosterFrame(bobWs, f) // grants adopt; sealed frames need the new key (adopted first)
	}
	gotKey, ok := dBob.currentKey(bobWs)
	if !ok || gotKey != [32]byte(mustKey(t, ws.Key)) {
		t.Fatal("Bob did not adopt the rotated key via roster ingest")
	}
	if bobWs.Epoch != ws.Epoch {
		t.Fatalf("Bob epoch=%d want %d", bobWs.Epoch, ws.Epoch)
	}
}

func mustKey(t *testing.T, b []byte) [32]byte {
	t.Helper()
	var k [32]byte
	if len(b) != 32 {
		t.Fatalf("bad key len %d", len(b))
	}
	copy(k[:], b)
	return k
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./fabric/daemon/ -run TestOpenerAdoptsRotatedKeyLive -v`. Expected: compile error (`currentKey` undefined) / failure (ingest ignores grants).

- [ ] **Step 3: Add `currentKey` + `persistWorkspaceKey`.** In `e2ectx.go`:

```go
// currentKey returns the workspace's current symmetric key, read under d.mu so
// the opener observes rotations applied by the roster-ingest loop.
func (d *Daemon) currentKey(ws *agentstate.Workspace) ([32]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(ws.Key) != 32 {
		return [32]byte{}, false
	}
	var k [32]byte
	copy(k[:], ws.Key)
	return k, true
}
```

In `daemon.go`:

```go
// persistWorkspaceKey best-effort saves ws's key/epoch to state.json after a
// roster-ingested rotation. No-op when statePath is unset (tests).
func (d *Daemon) persistWorkspaceKey(ws *agentstate.Workspace) {
	if d.statePath == "" {
		return
	}
	if err := agentstate.Save(d.statePath, d.state); err != nil {
		log.Printf("roster: persist workspace key: %v", err)
	}
}
```

- [ ] **Step 4: Re-read key per frame in `openerFor`.** In `inbox.go`, change the e2e closure (lines ~36-65) to fetch the key per frame instead of using captured `ec.key`:

```go
	return func(e envelope.Envelope) (envelope.Envelope, bool) {
		if e.Enc == "" {
			return envelope.Envelope{}, false
		}
		key, ok := d.currentKey(ec.ws) // re-read per frame so rotations take effect live
		if !ok {
			return envelope.Envelope{}, false
		}
		raw, derr := base64.StdEncoding.DecodeString(e.Enc)
		if derr != nil {
			return envelope.Envelope{}, false
		}
		env, perr := e2e.Parse(raw)
		if perr != nil {
			return envelope.Envelope{}, false
		}
		dev, counter, content, oerr := e2e.OpenMessage(key, ec.channelID, env, d.trust.resolve)
		if oerr != nil {
			return envelope.Envelope{}, false
		}
		if !d.replay.accept(replayKey{device: dev, channel: ec.channelID, epoch: env.KeyEpoch}, counter) {
			return envelope.Envelope{}, false
		}
		subj, body, cerr := decodeContent(content)
		if cerr != nil {
			return envelope.Envelope{}, false
		}
		e.Subject, e.Body, e.Enc = subj, body, ""
		return e, true
	}
```

Update `openerFor`'s doc comment: it captures the static e2e context once but re-reads the key per frame via `currentKey`.

- [ ] **Step 5: Adopt rekey grants in `ingestRosterFrame`.** Prepend a discriminator before the existing sealed-frame logic:

```go
func (d *Daemon) ingestRosterFrame(ws *agentstate.Workspace, b64 string) {
	// Rekey grants are published cleartext (JSON, starts with '{'); cert/anchors
	// frames are base64-sealed envelopes (never start with '{').
	if len(b64) > 0 && b64[0] == '{' {
		d.ingestRekeyGrant(ws, b64)
		return
	}
	key, err := keyArray(ws.Key)
	// ... existing openRosterFrame + cert/anchors switch unchanged ...
}

// ingestRekeyGrant adopts a signed rekey grant addressed to a local anchor.
func (d *Daemon) ingestRekeyGrant(ws *agentstate.Workspace, js string) {
	g, err := parseAdmitGrant([]byte(js))
	if err != nil || len(g.WrappedKey) == 0 {
		return
	}
	ag, ok := d.state.AgentByID(g.AnchorID)
	if !ok || len(ag.EncPriv) != 32 {
		return // not addressed to one of our local anchors
	}
	key, epoch, ok := ProcessRekey(g, ag.EncPriv, ws.AdminPub)
	if !ok {
		return
	}
	d.applyRekey(ws, key, epoch)
}

// applyRekey installs a newly-adopted key/epoch under d.mu and persists it.
func (d *Daemon) applyRekey(ws *agentstate.Workspace, key [32]byte, epoch uint32) {
	d.mu.Lock()
	if epoch >= ws.Epoch { // monotonic: never roll backwards
		ws.Key = key[:]
		ws.Epoch = epoch
	}
	d.mu.Unlock()
	d.persistWorkspaceKey(ws)
}
```

- [ ] **Step 6: Run tests with the race detector** — `go test ./fabric/daemon/ -race -run TestOpenerAdoptsRotatedKeyLive -v` then `go test ./fabric/daemon/ -race -v`. Expected: PASS, no race.

- [ ] **Step 7: Commit**

```bash
git add fabric/daemon/inbox.go fabric/daemon/e2ectx.go fabric/daemon/roster.go fabric/daemon/daemon.go fabric/daemon/rotation_live_test.go
git commit -m "feat(e2e): opener re-reads key per frame; roster ingest adopts rekeys"
```

---

### Task 4: Roster subscribe-loop

A long-running per-workspace loop that subscribes to `ws.Roster` and feeds every frame to `ingestRosterFrame`, so certs, anchor-set updates, and rekeys propagate without manual intervention.

**Files:**
- Create: `fabric/daemon/roster_loop.go`
- Modify: `fabric/daemon/daemon.go` (`RunOn`: start one `runRoster` per e2e workspace)
- Test: `fabric/daemon/roster_loop_test.go` (new)

**Interfaces:**
- Produces: `func runRoster(ctx context.Context, d *Daemon, ws *agentstate.Workspace)` — subscribe → `ingestRosterFrame` per frame → resume from cursor on disconnect (mirror `runInbox`'s reconnect/backoff structure). Reuses `sleepCtx`, `reconnectBackoff`.
- Consumes: `d.hub.Subscribe`, `ingestRosterFrame` (Task 3).

- [ ] **Step 1: Write the failing test** — `fabric/daemon/roster_loop_test.go`:

```go
package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"golang.org/x/crypto/nacl/box"
)

func TestRunRosterAdoptsRekey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Admin admits Bob; build Bob's daemon as the loop's host.
	dAdmin, fake, ws := newAdminDaemon(t)
	bobSignPub, bobSignSeed, _ := ed25519.GenerateKey(rand.Reader)
	bobEncPub, bobEncPriv, _ := box.GenerateKey(rand.Reader)
	_, _ = dAdmin.AdmitJoinRequest(ctx, ws, JoinRequest{ReqID: "bob", SignPub: bobSignPub, EncPub: bobEncPub[:]})

	bobState := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "bob", SignSeed: bobSignSeed.Seed(), EncPriv: bobEncPriv[:]}},
		Workspaces: []agentstate.Workspace{{RootID: "bob", E2E: true, Epoch: ws.Epoch, Key: append([]byte(nil), ws.Key...), AdminPub: append([]byte(nil), ws.AdminPub...), Roster: ws.Roster}},
	}
	dBob := &Daemon{state: bobState, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	bobWs := &bobState.Workspaces[0]

	go runRoster(ctx, dBob, bobWs) // subscribe BEFORE publishing (Fake has no replay)
	time.Sleep(20 * time.Millisecond)

	if _, err := dAdmin.RotateKey(ctx, ws); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k, ok := dBob.currentKey(bobWs); ok && k == mustKey(t, ws.Key) {
			return // adopted
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runRoster did not adopt the rotated key")
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./fabric/daemon/ -run TestRunRosterAdoptsRekey -v`. Expected: compile error (`runRoster` undefined).

- [ ] **Step 3: Implement `runRoster`** in `fabric/daemon/roster_loop.go`, mirroring `runInbox`'s subscribe/reconnect loop but calling `ingestRosterFrame(ws, fr.Body)` per frame and tracking `cursor`. (Read `runInbox` in `inbox.go` for the exact `Subscribe`/`select`/`sleepCtx` shape; the roster body is a single frame string, not a router batch, so no `unwrap`.)

```go
package daemon

import (
	"context"
	"log"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

// runRoster subscribes to ws.Roster and ingests every frame (certs, anchor-set
// updates, rekey grants) until ctx is cancelled, resuming from the latest cursor
// after a disconnect.
func runRoster(ctx context.Context, d *Daemon, ws *agentstate.Workspace) {
	cursor := ""
	for {
		if ctx.Err() != nil {
			return
		}
		frames, err := d.hub.Subscribe(ctx, ws.Roster, cursor)
		if err != nil {
			log.Printf("daemon: subscribe roster %s: %v", ws.Roster, err)
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
		disconnected := false
		for !disconnected {
			select {
			case <-ctx.Done():
				return
			case fr, ok := <-frames:
				if !ok {
					disconnected = true
					break
				}
				d.ingestRosterFrame(ws, fr.Body)
				if fr.Resume != "" {
					cursor = fr.Resume
				}
			}
		}
		if !sleepCtx(ctx, reconnectBackoff) {
			return
		}
	}
}
```

- [ ] **Step 4: Wire into `RunOn`.** Read `RunOn` in `daemon.go`. After it attaches agents (where it sets `d.serving`/`d.runCtx`), add a per-workspace startup that starts `runRoster` once per e2e workspace under `d.runCtx`, guarded by a dedupe set (e.g. a `map[string]context.CancelFunc` keyed by `ws.RootID`, mirroring `d.cancels`). For each `ws := range d.state.Workspaces` where `ws.E2E && ws.Roster != ""`, call `d.hydrateWorkspaceTrust(&d.state.Workspaces[i])` then `go runRoster(d.runCtx, d, &d.state.Workspaces[i])`. Take the address of the slice element (`&d.state.Workspaces[i]`), never a loop-variable copy.

- [ ] **Step 5: Run tests** — `go test ./fabric/daemon/ -race -v`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add fabric/daemon/roster_loop.go fabric/daemon/daemon.go fabric/daemon/roster_loop_test.go
git commit -m "feat(e2e): roster subscribe-loop (auto-ingest certs/anchors/rekeys)"
```

---

### Task 5: Waiting-room subscribe-loop + persisted pending requests

The admin host's loop subscribes to `ws.WaitingRoom`, decodes `JoinRequest`s (ignoring `AdmitGrant`s, which also land there), dedupes by `ReqID`, and persists them to `ws.Pending` so the one-shot `pending`/`admit` CLI commands can act on them.

**Files:**
- Modify: `fabric/agentstate/agentstate.go` (`Workspace.Pending []PendingJoin`)
- Create: `fabric/daemon/waitingroom_loop.go`
- Modify: `fabric/daemon/daemon.go` (`RunOn`: start `runWaitingRoom` for e2e workspaces we admin)
- Test: `fabric/daemon/waitingroom_loop_test.go` (new)

**Interfaces:**
- Produces:
  - `agentstate.PendingJoin{ReqID, Name, ParentIntent string; SignPub, EncPub []byte}` (json) — a persisted copy of an inbound `JoinRequest`. (Mirror of `daemon.JoinRequest`; kept in `agentstate` to avoid an import cycle.)
  - `Workspace.Pending []PendingJoin` (json `pending,omitempty`).
  - `func runWaitingRoom(ctx context.Context, d *Daemon, ws *agentstate.Workspace)` — subscribe → decode JoinRequest → `d.recordPending(ws, req)` → persist.
  - `func (d *Daemon) recordPending(ws *agentstate.Workspace, req JoinRequest)` — dedupe by ReqID, append, save (under `d.mu`).
- Consumes: `parseJoinRequest`, `d.hub.Subscribe`.

- [ ] **Step 1: Write the failing test** — `fabric/daemon/waitingroom_loop_test.go`:

```go
package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"
)

func TestRunWaitingRoomRecordsPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, fake, ws := newAdminDaemon(t)

	go runWaitingRoom(ctx, d, ws)
	time.Sleep(20 * time.Millisecond)

	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	encPub, _, _ := box.GenerateKey(rand.Reader)
	jb, _ := JoinRequest{ReqID: "r1", Name: "alice-laptop", SignPub: signPub, EncPub: encPub[:]}.Marshal()
	fake.Publish(ctx, ws.WaitingRoom, string(jb))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(ws.Pending) == 1 && ws.Pending[0].ReqID == "r1" {
			// A grant on the same channel must NOT be recorded as pending.
			gb, _ := AdmitGrant{ReqID: "r1", AnchorID: "r1"}.Marshal()
			fake.Publish(ctx, ws.WaitingRoom, string(gb))
			time.Sleep(50 * time.Millisecond)
			if len(ws.Pending) != 1 {
				t.Fatalf("grant polluted pending: %+v", ws.Pending)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("join request not recorded: %+v", ws.Pending)
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./fabric/daemon/ -run TestRunWaitingRoomRecordsPending -v`. Expected: compile error.

- [ ] **Step 3: Add the model + loop.** In `agentstate.go` add `PendingJoin` and `Workspace.Pending`. In `waitingroom_loop.go`:

```go
package daemon

import (
	"context"
	"log"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

func runWaitingRoom(ctx context.Context, d *Daemon, ws *agentstate.Workspace) {
	cursor := ""
	for {
		if ctx.Err() != nil {
			return
		}
		frames, err := d.hub.Subscribe(ctx, ws.WaitingRoom, cursor)
		if err != nil {
			log.Printf("daemon: subscribe waiting-room %s: %v", ws.WaitingRoom, err)
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
		disconnected := false
		for !disconnected {
			select {
			case <-ctx.Done():
				return
			case fr, ok := <-frames:
				if !ok {
					disconnected = true
					break
				}
				req, perr := parseJoinRequest([]byte(fr.Body))
				if perr == nil && req.ReqID != "" && len(req.SignPub) > 0 && len(req.EncPub) > 0 {
					d.recordPending(ws, req)
				}
				if fr.Resume != "" {
					cursor = fr.Resume
				}
			}
		}
		if !sleepCtx(ctx, reconnectBackoff) {
			return
		}
	}
}

func (d *Daemon) recordPending(ws *agentstate.Workspace, req JoinRequest) {
	d.mu.Lock()
	for _, p := range ws.Pending {
		if p.ReqID == req.ReqID {
			d.mu.Unlock()
			return
		}
	}
	ws.Pending = append(ws.Pending, agentstate.PendingJoin{
		ReqID: req.ReqID, Name: req.Name, ParentIntent: req.ParentIntent,
		SignPub: append([]byte(nil), req.SignPub...), EncPub: append([]byte(nil), req.EncPub...),
	})
	d.mu.Unlock()
	d.persistWorkspaceKey(ws) // reuse the best-effort state.json save
}
```

> Note: an `AdmitGrant` JSON also unmarshals into `JoinRequest` with empty `SignPub`/`EncPub`; the `len(req.SignPub) > 0 && len(req.EncPub) > 0` guard rejects it. Keep that guard.

- [ ] **Step 4: Wire into `RunOn`.** Alongside the Task 4 roster-loop startup, for each e2e workspace where `len(ws.AdminPriv) > 0` (we are the admin), `go runWaitingRoom(d.runCtx, d, &d.state.Workspaces[i])`.

- [ ] **Step 5: Run tests** — `go test ./fabric/daemon/ -race -v`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add fabric/agentstate/agentstate.go fabric/daemon/waitingroom_loop.go fabric/daemon/daemon.go fabric/daemon/waitingroom_loop_test.go
git commit -m "feat(e2e): waiting-room subscribe-loop persists pending join requests"
```

---

### Task 6: CLI `workspace pending`

List the active (or named) e2e workspace's pending join requests with their SAS fingerprints for out-of-band verification.

**Files:**
- Modify: `cmd/botbus/workspace.go` (dispatch + `workspacePending`)
- Test: `cmd/botbus/workspace_pending_test.go` (new)

**Interfaces:**
- Produces: `func workspacePending(statePath, wsName string) (string, error)` — returns formatted lines (one per pending request: `reqId  name  SAS  parentIntent`). Printing is done by the dispatcher. `wsName == ""` ⇒ active workspace.
- Consumes: `agentstate.Load`, `daemon.sasFingerprint` (export or replicate — see step 3), `Workspace.Pending` (Task 5).

- [ ] **Step 1: Write the failing test** — `cmd/botbus/workspace_pending_test.go`. Write a temp `state.json` with one workspace + one `Pending` entry, call `workspacePending`, assert the output contains the reqId, name, and a `XXXX-XXXX-XXXX`-shaped SAS.

```go
package main

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

func TestWorkspacePendingLists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := &agentstate.State{
		ActiveWorkspace: "root1",
		Workspaces: []agentstate.Workspace{{
			RootID: "root1", E2E: true,
			Pending: []agentstate.PendingJoin{{ReqID: "r1", Name: "alice-laptop", SignPub: make([]byte, 32), EncPub: make([]byte, 32)}},
		}},
	}
	if err := agentstate.Save(path, st); err != nil {
		t.Fatal(err)
	}
	out, err := workspacePending(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`r1`).MatchString(out) || !regexp.MustCompile(`alice-laptop`).MatchString(out) {
		t.Fatalf("missing request fields: %q", out)
	}
	if !regexp.MustCompile(`[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}`).MatchString(out) {
		t.Fatalf("missing SAS fingerprint: %q", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails** — `go test ./cmd/botbus/ -run TestWorkspacePendingLists -v`. Expected: compile error.

- [ ] **Step 3: Export `sasFingerprint`.** Rename `daemon.sasFingerprint` → `daemon.SASFingerprint` (it must be callable from `cmd/botbus`). Update its one internal caller (if any) and add a doc comment. (Per no-legacy: rename in place, update callers, same commit.)

- [ ] **Step 4: Implement `workspacePending`** in `workspace.go`: load state, resolve the workspace (active or named) via the existing lookup pattern, build one line per `Pending` entry using `daemon.SASFingerprint(p.SignPub, p.EncPub)`. Add `case "pending":` to `workspaceCmd` that calls it and prints, with a friendly "no pending requests" when empty.

- [ ] **Step 5: Run tests** — `go test ./cmd/botbus/ ./fabric/daemon/ -v`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/botbus/workspace.go cmd/botbus/workspace_pending_test.go fabric/daemon/admission.go
git commit -m "feat(cli): workspace pending — list join requests with SAS"
```

---

### Task 7: CLI `workspace admit <reqId>`

Admit a pending request: reconstruct a hydrated admin `Daemon`, call `AdmitJoinRequest`, drop the request from `ws.Pending`, and save.

**Files:**
- Modify: `cmd/botbus/workspace.go` (dispatch + `workspaceAdmit`)
- Test: `cmd/botbus/workspace_admit_test.go` (new)

**Interfaces:**
- Produces: `func workspaceAdmit(ctx context.Context, d hostagent.Deps, wsName, reqID string) error` — loads state, builds a `*daemon.Daemon` via `daemon.NewRuntime(daemon.Config{State, StatePath, Hub})`, `hydrateWorkspaceTrust`, finds the pending request, `AdmitJoinRequest`, removes from `ws.Pending`, `agentstate.Save`.
- Consumes: `daemon.NewRuntime`, `daemon.Config`, `hydrateWorkspaceTrust` (export as `HydrateWorkspaceTrust` or add a thin exported wrapper — see step 3), `AdmitJoinRequest`.

- [ ] **Step 1: Write the failing test** — `cmd/botbus/workspace_admit_test.go`. Build a state with an e2e admin workspace (key + AdminPriv + Roster + WaitingRoom + one `Pending`), a `hubclient.Fake` in `Deps`, call `workspaceAdmit`, then assert: `ws.Anchors` gained the anchor, `ws.Pending` is empty, and a grant was published to the waiting room (`fake.Published(waitingRoom)` non-empty, parses as an `AdmitGrant` with the right `AnchorID`).

- [ ] **Step 2: Run test to verify it fails** — `go test ./cmd/botbus/ -run TestWorkspaceAdmit -v`. Expected: compile error.

- [ ] **Step 3: Export the hydration entry point.** Add an exported method `func (d *Daemon) HydrateWorkspaceTrust(ws *agentstate.Workspace)` that calls the unexported `hydrateWorkspaceTrust` (or export the original). The CLI must hydrate before admit so the re-published anchor set includes all prior anchors.

- [ ] **Step 4: Implement `workspaceAdmit`** + `case "admit":` (parse `<reqId>` and optional `--workspace`). Reconstruct the request from `ws.Pending` (its `SignPub`/`EncPub`), call `AdmitJoinRequest`, remove the entry from `ws.Pending`, `Save`. Print the admitted anchorId + new anchor count.

- [ ] **Step 5: Run tests** — `go test ./cmd/botbus/ ./fabric/daemon/ -v`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/botbus/workspace.go cmd/botbus/workspace_admit_test.go fabric/daemon/daemon.go
git commit -m "feat(cli): workspace admit <reqId>"
```

---

### Task 8: CLI `workspace key-rotate` + `workspace remove <anchorId>`

The two admin lifecycle commands, both reconstructing a hydrated admin `Daemon` and persisting the rotated key/epoch + updated anchors.

**Files:**
- Modify: `cmd/botbus/workspace.go` (dispatch + `workspaceKeyRotate`, `workspaceRemove`)
- Test: `cmd/botbus/workspace_lifecycle_test.go` (new)

**Interfaces:**
- Produces:
  - `func workspaceKeyRotate(ctx context.Context, d hostagent.Deps, wsName string) error`
  - `func workspaceRemove(ctx context.Context, d hostagent.Deps, wsName, anchorID string) error`
  - Both: load state → `NewRuntime` → `HydrateWorkspaceTrust` → `RotateKey`/`RemoveAnchor` → `Save`.
- Consumes: `RotateKey`, `RemoveAnchor` (Tasks 1-2).

- [ ] **Step 1: Write the failing test** — `cmd/botbus/workspace_lifecycle_test.go`:
  - `TestWorkspaceKeyRotateBumpsEpoch`: admin ws at epoch 1 with one persisted anchor → `workspaceKeyRotate` → reloaded state shows epoch 2, a different key, and a rekey grant for the anchor was published to the roster.
  - `TestWorkspaceRemoveEvictsAndRotates`: ws with two anchors → `workspaceRemove(..., "anchorB")` → reloaded state: epoch bumped, `ws.Anchors` no longer contains `anchorB`, and a rekey grant was published for the surviving anchor but **not** for `anchorB`.

- [ ] **Step 2: Run test to verify it fails** — `go test ./cmd/botbus/ -run 'TestWorkspaceKeyRotate|TestWorkspaceRemove' -v`. Expected: compile error.

- [ ] **Step 3: Implement** both functions + dispatch. `case "key-rotate":` (note the hyphen — single token) and `case "remove":` (parse `<anchorId>` + optional `--workspace`). After the operation, `agentstate.Save(d.StatePath, st)`.

- [ ] **Step 4: Run tests** — `go test ./cmd/botbus/ ./fabric/daemon/ -v`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/botbus/workspace.go cmd/botbus/workspace_lifecycle_test.go
git commit -m "feat(cli): workspace key-rotate + workspace remove <anchorId>"
```

---

### Task 9: CLI `workspace join <url|handle>`

On a joiner host: generate keypairs, post a `JoinRequest` (printing the joiner's SAS for the admin to confirm out-of-band), subscribe to the waiting room, await the matching signed `AdmitGrant`, `ProcessAdmitGrant` (TOFU-pin the admin pubkey on first contact), and persist the new local agent + workspace.

**Files:**
- Modify: `cmd/botbus/workspace.go` (dispatch + `workspaceJoin`)
- Modify: `fabric/daemon/admission.go` (allow TOFU in `verifyGrant` when `expectedAdminPub == nil`)
- Test: `cmd/botbus/workspace_join_test.go` (new); `fabric/daemon/admission_tofu_test.go` (new)

**Interfaces:**
- Produces:
  - `func workspaceJoin(ctx context.Context, d hostagent.Deps, target, name string) error` — resolves the waiting-room channel from `target` (a `…/join` URL or a bare channel handle — extract the channel id; reuse any existing URL/handle parsing in `workspace.go`/`workspaceInvite`), mints `SignSeed` + `EncPriv` (reuse `hostagent.Create`'s e2e keygen path or its helpers `newSignSeed`/`newEncKey`), uses a freshly-minted agent ID as the `JoinRequest.ReqID`, posts it, prints `SASFingerprint`, subscribes, waits (with a ctx timeout) for an `AdmitGrant` whose `ReqID` matches, calls `ProcessAdmitGrant(grant, encPriv, nil)`, writes the agent (`ID == ReqID`, `SignSeed`, `EncPriv`, an inbox channel) + the returned workspace into state, saves.
- Consumes: `daemon.ProcessAdmitGrant`, `daemon.JoinRequest`, `daemon.SASFingerprint`, `d.Hub.Subscribe/Publish/MintChannel`.

- [ ] **Step 1: Write the failing tests.**
  - `fabric/daemon/admission_tofu_test.go` — `TestProcessAdmitGrantTOFU`: a validly-signed grant with `expectedAdminPub == nil` is accepted and pins `grant.AdminPub`; a grant whose `Sig` is corrupted is rejected even under TOFU.
  - `cmd/botbus/workspace_join_test.go` — `TestWorkspaceJoinCompletes`: with a `hubclient.Fake`, run `workspaceJoin` in a goroutine; once it has posted its request (poll `fake.Published(waitingRoom)` for a `JoinRequest`), simulate the admin by building an `AdmitGrant` for that `ReqID` (wrap the workspace key to the posted `EncPub`, sign with a test admin key) and `fake.Publish` it to the waiting room; assert `workspaceJoin` returns nil and the saved state contains the workspace (correct `Key`, `RootID`, `AdminPub`) + the local agent (`ID == ReqID`, has `EncPriv`).

- [ ] **Step 2: Run tests to verify they fail** — `go test ./cmd/botbus/ -run TestWorkspaceJoinCompletes -v` and `go test ./fabric/daemon/ -run TestProcessAdmitGrantTOFU -v`. Expected: compile errors.

- [ ] **Step 3: Allow TOFU in `verifyGrant`.** Change the admin-pub check so that when `expectedAdminPub == nil` (length 0) the grant is verified **against its own `AdminPub`** (and `grant.AdminPub` must be a valid 32-byte ed25519 key); when `expectedAdminPub` is non-nil it must match (unchanged). Keep the signature check mandatory in both cases.

```go
	switch {
	case len(expectedAdminPub) == 0:
		if len(grant.AdminPub) != ed25519.PublicKeySize {
			return [32]byte{}, false
		}
	case !bytes.Equal(grant.AdminPub, expectedAdminPub):
		return [32]byte{}, false
	}
```

> This relaxes ONLY `ProcessAdmitGrant` first-contact (join). `ProcessRekey` (Task 3 ingest) always passes a non-nil pinned `ws.AdminPub`, so rekeys remain strictly authenticated. Update `ProcessAdmitGrant`'s and `verifyGrant`'s doc comments to describe TOFU.

- [ ] **Step 4: Implement `workspaceJoin`** + `case "join":` (parse `<url|handle>` + optional `--name`). Use a bounded `context.WithTimeout` for the wait so the command can't hang forever; on timeout, return a clear error ("no admit grant received — ask the admin to run `workspace admit <reqId>`"). Print the `reqId` + SAS before waiting.

- [ ] **Step 5: Run tests** — `go test ./cmd/botbus/ ./fabric/daemon/ -v`. Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/botbus/workspace.go cmd/botbus/workspace_join_test.go fabric/daemon/admission.go fabric/daemon/admission_tofu_test.go
git commit -m "feat(cli): workspace join <url|handle> (post request, TOFU-pin admin, adopt key)"
```

---

### Task 10: Two-host end-to-end integration tests

Prove the spec's self-verification targets against the wired loops, with a single `hubclient.Fake` shared between an admin daemon and a joiner daemon.

**Files:**
- Create: `fabric/daemon/cross_host_runnable_test.go`

**Interfaces:** Consumes everything above. No new production code (if a test reveals a gap, fix it in the relevant task's file and note it).

- [ ] **Step 1: Write the tests.**
  - `TestJoinAdmitConvergeTwoHosts`: admin + joiner share a `Fake`. Start both daemons' `runRoster`; start admin's `runWaitingRoom`. Joiner posts a `JoinRequest`; admin (drive `recordPending` via the loop, then call `AdmitJoinRequest` directly — or call the `workspaceAdmit` path) admits. Then the joiner sends an e2e message to a channel the admin's opener reads, and the admin decrypts it; assert the admin's `openerFor(joinerAgent)`/`Send` round-trips. Assert relay-blind on the message body.
  - `TestRotateConvergesAtNewEpoch`: after a join+admit, admin `RotateKey`; the joiner's `runRoster` adopts; a message the joiner sends post-rotation decrypts under the new epoch on the admin (and vice-versa). Assert `joinerWs.Epoch` advanced.
  - `TestRemoveEvictsFromNewEpoch`: admit two joiners (A, B); `RemoveAnchor(B)`; A's roster loop adopts the new key and still converges; **B's** `currentKey` does **not** advance to the new key (no rekey grant was wrapped to B), so B cannot decrypt a new-epoch message. Assert B's opener drops/fails on a post-removal frame.
  - In each: assert no plaintext key bytes and no plaintext message bodies appear in `fake.Published(...)` for any channel.

- [ ] **Step 2: Run** — `go test ./fabric/daemon/ -race -run 'TestJoinAdmitConvergeTwoHosts|TestRotateConvergesAtNewEpoch|TestRemoveEvictsFromNewEpoch' -v`. Expected: PASS. Fix any production gap in its owning file; re-run.

- [ ] **Step 3: Full suite** — `go build ./... && go test ./... -race`. Expected: PASS (the pre-existing `cmd/botbus` `TestNameColor` failure, if still present on `main`, is unrelated to this work — confirm it predates this branch with `git stash && go test ./cmd/botbus/ -run TestNameColor`; if it's a pre-existing red, note it and exclude it from the green-bar claim).

- [ ] **Step 4: Commit**

```bash
git add fabric/daemon/cross_host_runnable_test.go
git commit -m "test(e2e): two-host join/admit/rotate/remove convergence + relay-blind"
```

---

### Task 11: README status + docs

Flip the README's "remaining wiring" status to shipped and document the one honest limitation.

**Files:**
- Modify: `README.md` (cross-host section, ~lines 207-233)

- [ ] **Step 1:** Update the cross-host section: list the now-available CLI commands (`workspace join`/`pending`/`admit`/`key-rotate`/`remove`) with one-line usage each, and state that the roster + waiting-room subscribe-loops run inside `botbus daemon`. Add a short **Limitation** note: after running `workspace key-rotate`/`remove`/`admit` as a one-shot command on the **admin's own** host, restart that host's `botbus daemon` so its in-memory key/anchor set picks up the change (remote hosts adopt automatically via the roster loop; same-host daemon auto-reload is a documented follow-up). Note also that admit does **not** rotate the key (only `remove` and `key-rotate` do).

- [ ] **Step 2:** No tests for docs. Sanity-check links/commands render.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs(e2e): cross-host CLI + subscribe-loops shipped; same-host reload note"
```

---

## Self-Review (completed at plan-authoring time)

- **Spec coverage:** Admission flow (join/pending/admit) → Tasks 5-7,9. Key distribution wrap-to-anchor → Tasks 1,2. Rotate-on-membership-change (remove) + explicit rotate → Tasks 2,8. Removal evicts subtree → Tasks 2,8,10. "Relay sees only ciphertext for bodies and wrapped key blobs" → asserted in Tasks 2,10. "Removal rolls epoch; removed host cannot decrypt" → Task 10. Subscribe-loops → Tasks 4,5. Opener live-rotation (I-2) → Task 3. Anchor persistence (I-3) → Task 1. Admit-does-not-rotate (maintainer decision) → preserved (Task 1 keeps `AdmitJoinRequest` rotation-free; only `RemoveAnchor`/`RotateKey` rotate).
- **Deferred (documented, not silently dropped):** same-host daemon auto-reload after a one-shot admin command (Task 11 limitation note); state.json write-contention between the long-running daemon and one-shot CLI (M-1) — admin ops are rare and human-driven; `RotateKey` builds rekey frames before mutating published anchor state (M-4) is satisfied by Task 2's ordering. Per-message forward secrecy and same-host read-segregation remain spec non-goals.
- **Type consistency:** `AdmitGrant`/`JoinRequest` field names match `admission.go`. `currentKey`/`applyRekey`/`ingestRekeyGrant`/`recordPending`/`hydrateWorkspaceTrust`/`HydrateWorkspaceTrust`/`persistWorkspaceKey`/`runRoster`/`runWaitingRoom`/`ProcessRekey`/`verifyGrant` are each defined once and referenced consistently. `Workspace.Anchors []AnchorRef` and `Workspace.Pending []PendingJoin` are defined in Tasks 1 and 5 respectively and consumed thereafter.
- **No placeholders:** every code step shows concrete code or a precise edit; test steps show real assertions.
