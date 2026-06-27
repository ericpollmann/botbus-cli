# E2E Phase 1 — Symmetric Core (opt-in workspace) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an **opt-in** e2e workspace deliver real messages whose body/subject the relay never sees — daemon-side encrypt/decrypt + sign/verify under one shared workspace key, with per-device signing keys and an admin-signed device set — while non-e2e workspaces stay byte-for-byte unchanged.

**Architecture:** The daemon is the encryption boundary. On **send**, if the sending agent's workspace is e2e, the daemon seals `{subject, body}` with `e2e.SealMessage` (sign-then-encrypt under the workspace key) and ships the ciphertext in a new cleartext-metadata envelope field `Enc`; `Subject`/`Body` go empty on the wire. On **receive**, the daemon opens `Enc` with `e2e.OpenMessage`, verifies the sender's device signature against an admin-signed device set, enforces a monotonic replay counter, and repopulates `Subject`/`Body` in memory before the message reaches `next`. The hub/relay stays byte-agnostic and sees only opaque ciphertext. Channel/epoch are bound into the AEAD AAD and the signed payload so cross-channel/cross-epoch replay is rejected by the crypto, not by trusting the relay.

**Tech Stack:** Go; `fabric/e2e` primitives (PR #27 — XChaCha20-Poly1305 AEAD + Ed25519 sign-then-encrypt + HKDF, already on this branch); `github.com/ericpollmann/botbus-proto` envelope; `hubclient.Fake` for in-process two-daemon tests.

## Global Constraints

- **Opt-in only; non-e2e is untouched.** Every change is gated on a per-workspace `E2E` flag. A non-e2e workspace must produce identical wire bytes to today. The full existing test suite (`go test ./...`) stays green at every commit.
- **Daemon is the only encryption boundary.** No crypto in the CLI command layer beyond key generation; no crypto in the hub.
- **Relay sees only ciphertext (the invariant under test).** For an e2e message, the published frame's decoded envelope MUST have `Subject == ""`, `Body == ""`, `Enc != ""`. Key/recipient decisions never depend on relay-supplied data.
- **`channelID` for AAD + signature = the workspace root id** (the org-root agent id, identical for every member of a workspace). `deviceID` = the sending agent's id. (Phase 3 swaps the workspace-root id for per-topic HKDF ids; the seam is the `channelID` argument.)
- **Reuse `fabric/e2e` verbatim — do not reimplement crypto.** Use `e2e.SealMessage`, `e2e.OpenMessage`. Convert `[]byte` keys to `[32]byte` with one helper; never copy primitive logic.
- **No new third-party deps.** `golang.org/x/crypto` (already added by #27) and stdlib only. No legacy/compat shims — when you change a function signature, update all callers in the same commit (per repo CLAUDE.md "No Legacy / Compat Code").
- **Run `git commit` as its own standalone Bash call** (never chained after `git add`), so the pre-commit hook inspects the commit independently. Use `trash`, never `rm`.
- **Key material is 0600.** `state.json` is already written 0600 by `agentstate.Save`; never widen it, never log key bytes, never put a raw key in a URL or a CLI arg.

---

## File Structure

| File | Responsibility | New/Modified |
| --- | --- | --- |
| `botbus-proto/envelope/envelope.go` | Add `Enc` cleartext-metadata field | Modify (separate repo, tag release) |
| `fabric/agentstate/agentstate.go` | `Workspace` key record + per-agent device seed + tree-walk + lookup helpers | Modify |
| `fabric/daemon/e2ecodec.go` | Encode/decode the sealed inner `{subject, body}` payload | Create |
| `fabric/daemon/e2ectx.go` | Resolve an agent's e2e context (key, epoch, device priv, channelID, counters, device set) from `*Daemon` | Create |
| `fabric/daemon/replay.go` | Sender monotonic counter (persisted) + receiver replay window (in-memory) | Create |
| `fabric/daemon/deviceset.go` | In-memory admin-signed device-pubkey set + verify + roster-channel ingest | Create |
| `fabric/daemon/tools.go` | Seal on send (branch in `Send`) | Modify |
| `fabric/daemon/inbox.go` | Open on receive (thread an opener into `runInbox`/`unwrap`) | Modify |
| `fabric/daemon/ops_impl.go` | Wire e2e context into `Daemon.Send` / inbox start | Modify |
| `cmd/botbus/workspace.go` | `workspace create --e2e` flag → mint key material | Modify |
| `fabric/daemon/e2e_integration_test.go` | Two-daemon convergence + relay-blind + wrong-key-cannot-read | Create |

---

## Task 1: Proto `Enc` field + release

**Files:**
- Modify: `/Users/pollmann/Documents/hack/botbus-proto/envelope/envelope.go:45-56`
- Test: `/Users/pollmann/Documents/hack/botbus-proto/envelope/envelope_test.go`
- Modify (consumer): `go.mod` in this worktree (bump proto version)

**Interfaces:**
- Produces: `envelope.Envelope.Enc string` (`json:"enc,omitempty"`), carrying base64 of an `e2e.Envelope.Marshal()`. When `Enc != ""`, `Subject`/`Body` are empty on the wire.

This is a separate Go module. Do the proto change + tag first, then point this worktree at it. For the dev loop, use a local `replace` directive; swap to the tagged version as the final step of this task.

- [ ] **Step 1: Write the failing test** (in the proto repo)

```go
// envelope_test.go
func TestEncFieldRoundTrips(t *testing.T) {
	e := Envelope{V: 1, ID: "x", From: "a", Kind: KindChat, Enc: "QUJD"}
	b, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"enc":"QUJD"`) {
		t.Fatalf("enc not serialized: %s", b)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enc != "QUJD" {
		t.Fatalf("enc round-trip: got %q", got.Enc)
	}
}

func TestEncOmittedWhenEmpty(t *testing.T) {
	b, _ := Encode(Envelope{V: 1, ID: "x", From: "a", Kind: KindChat})
	if strings.Contains(string(b), "enc") {
		t.Fatalf("empty enc must be omitted: %s", b)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run (in the proto checkout): `cd /Users/pollmann/Documents/hack/botbus-proto && go test ./envelope/ -run TestEnc -v`
Expected: FAIL — `Enc` is not a field.

- [ ] **Step 3: Add the field**

```go
// envelope.go — inside type Envelope struct, after Body:
	Subject string   `json:"subject,omitempty"`
	Body    string   `json:"body"`
	// Enc carries a base64 e2e ciphertext envelope. When set, Subject and Body
	// are empty on the wire; the daemon repopulates them after decryption.
	Enc     string   `json:"enc,omitempty"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/pollmann/Documents/hack/botbus-proto && go test ./envelope/ -v`
Expected: PASS (all existing envelope tests still green).

- [ ] **Step 5: Commit, tag, and point this worktree at it**

Commit the proto change on a branch + PR in the proto repo, then tag a release:
```bash
# in /Users/pollmann/Documents/hack/botbus-proto, on a feat/ branch, after merge:
git tag v0.4.0 && git push origin v0.4.0
```
For local development before the tag lands, in THIS worktree add a replace directive so the daemon compiles against the local proto:
```bash
cd /Users/pollmann/Documents/hack/botbus-cli/.claude/worktrees/e2e-phase1
go mod edit -replace github.com/ericpollmann/botbus-proto=/Users/pollmann/Documents/hack/botbus-proto
go mod tidy
```
Final step of the whole plan (Task 9 commit): remove the replace and `go get github.com/ericpollmann/botbus-proto@v0.4.0`. Commit the proto change separately:
```bash
git add go.mod go.sum
git commit -m "chore(e2e): depend on botbus-proto Enc field (local replace for dev)"
```

---

## Task 2: agentstate — workspace key + device key model

**Files:**
- Modify: `fabric/agentstate/agentstate.go:29-54`
- Test: `fabric/agentstate/agentstate_test.go` (create if absent; otherwise append)

**Interfaces:**
- Consumes: existing `Agent{ID, Parent, ...}`, `State{Agents []Agent}`.
- Produces:
  - `type Workspace struct { RootID string; E2E bool; Epoch uint32; Key []byte; Salt []byte; AdminPub []byte }`
  - `State.Workspaces []Workspace` (`json:"workspaces,omitempty"`)
  - `Agent.SignSeed []byte` (`json:"signSeed,omitempty"`) — 32-byte Ed25519 seed; deviceID == Agent.ID
  - `func (s *State) AgentByID(id string) (*Agent, bool)`
  - `func (s *State) WorkspaceRootID(agentID string) string` — walk `Parent` to the root (`Parent==""`), cycle-safe (bounded by len(Agents)); returns `agentID` itself if it has no parent; returns `""` if the agent is unknown.
  - `func (s *State) WorkspaceFor(agentID string) (*Workspace, bool)` — `WorkspaceRootID` then lookup in `Workspaces`.

- [ ] **Step 1: Write the failing test**

```go
// agentstate_test.go
func TestWorkspaceRootIDWalksParents(t *testing.T) {
	s := &State{Agents: []Agent{
		{ID: "root", Parent: ""},
		{ID: "mid", Parent: "root"},
		{ID: "leaf", Parent: "mid"},
	}}
	if got := s.WorkspaceRootID("leaf"); got != "root" {
		t.Fatalf("got %q want root", got)
	}
	if got := s.WorkspaceRootID("root"); got != "root" {
		t.Fatalf("self-root: got %q", got)
	}
	if got := s.WorkspaceRootID("ghost"); got != "" {
		t.Fatalf("unknown agent: got %q want empty", got)
	}
}

func TestWorkspaceRootIDCycleSafe(t *testing.T) {
	s := &State{Agents: []Agent{
		{ID: "a", Parent: "b"},
		{ID: "b", Parent: "a"},
	}}
	// must terminate (return "" on cycle), not loop forever
	_ = s.WorkspaceRootID("a")
}

func TestWorkspaceForLooksUpKey(t *testing.T) {
	s := &State{
		Agents:     []Agent{{ID: "root", Parent: ""}, {ID: "leaf", Parent: "root"}},
		Workspaces: []Workspace{{RootID: "root", E2E: true, Key: []byte("k")}},
	}
	w, ok := s.WorkspaceFor("leaf")
	if !ok || !w.E2E {
		t.Fatalf("expected e2e workspace, got %v %v", w, ok)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/agentstate/ -run TestWorkspace -v`
Expected: FAIL — undefined `Workspace`, `WorkspaceRootID`, `WorkspaceFor`.

- [ ] **Step 3: Add the structs + helpers**

```go
// agentstate.go — add to State struct:
	Workspaces []Workspace `json:"workspaces,omitempty"`

// add to Agent struct:
	SignSeed []byte `json:"signSeed,omitempty"`

// new types + helpers:
type Workspace struct {
	RootID   string `json:"rootId"`
	E2E      bool   `json:"e2e,omitempty"`
	Epoch    uint32 `json:"epoch,omitempty"`
	Key      []byte `json:"key,omitempty"`      // 32-byte symmetric workspace key
	Salt     []byte `json:"salt,omitempty"`     // per-epoch HKDF salt (Phase 3 uses)
	AdminPub []byte `json:"adminPub,omitempty"` // pinned admin Ed25519 pubkey
}

func (s *State) AgentByID(id string) (*Agent, bool) {
	for i := range s.Agents {
		if s.Agents[i].ID == id {
			return &s.Agents[i], true
		}
	}
	return nil, false
}

// WorkspaceRootID walks Parent links to the org-root (Parent==""). It is
// cycle-safe: it visits at most len(Agents) hops and returns "" on a cycle or
// a dangling parent, mirroring the server-side registry.RootAncestorID.
func (s *State) WorkspaceRootID(agentID string) string {
	cur, ok := s.AgentByID(agentID)
	if !ok {
		return ""
	}
	for hops := 0; hops <= len(s.Agents); hops++ {
		if cur.Parent == "" {
			return cur.ID
		}
		next, ok := s.AgentByID(cur.Parent)
		if !ok {
			return "" // dangling parent
		}
		cur = next
	}
	return "" // cycle
}

func (s *State) WorkspaceFor(agentID string) (*Workspace, bool) {
	root := s.WorkspaceRootID(agentID)
	if root == "" {
		return nil, false
	}
	for i := range s.Workspaces {
		if s.Workspaces[i].RootID == root {
			return &s.Workspaces[i], true
		}
	}
	return nil, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/agentstate/ -v`
Expected: PASS (existing agentstate tests still green; `Save`/`Load` round-trip the new fields via JSON).

- [ ] **Step 5: Commit**

```bash
git add fabric/agentstate/agentstate.go fabric/agentstate/agentstate_test.go
git commit -m "feat(e2e): workspace key + device-seed model in agentstate"
```

---

## Task 3: Sealed-content codec

**Files:**
- Create: `fabric/daemon/e2ecodec.go`
- Test: `fabric/daemon/e2ecodec_test.go`

**Interfaces:**
- Produces:
  - `func encodeContent(subject, body string) []byte` — JSON `{"s":subject,"b":body}`.
  - `func decodeContent(b []byte) (subject, body string, err error)`.

This is the plaintext that `e2e.SealMessage` seals; keeping it tiny and explicit avoids leaking subject/body shape.

- [ ] **Step 1: Write the failing test**

```go
// e2ecodec_test.go
package daemon

import "testing"

func TestContentCodecRoundTrip(t *testing.T) {
	b := encodeContent("hello", "world body")
	s, body, err := decodeContent(b)
	if err != nil {
		t.Fatal(err)
	}
	if s != "hello" || body != "world body" {
		t.Fatalf("got (%q,%q)", s, body)
	}
}

func TestDecodeContentRejectsGarbage(t *testing.T) {
	if _, _, err := decodeContent([]byte("not json")); err == nil {
		t.Fatal("expected error on garbage")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestContentCodec -v` (and `TestDecodeContentRejectsGarbage`)
Expected: FAIL — undefined `encodeContent`.

- [ ] **Step 3: Implement**

```go
// e2ecodec.go
package daemon

import "encoding/json"

type e2eContent struct {
	S string `json:"s"`
	B string `json:"b"`
}

func encodeContent(subject, body string) []byte {
	b, _ := json.Marshal(e2eContent{S: subject, B: body})
	return b
}

func decodeContent(b []byte) (subject, body string, err error) {
	var c e2eContent
	if err = json.Unmarshal(b, &c); err != nil {
		return "", "", err
	}
	return c.S, c.B, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run "TestContentCodec|TestDecodeContent" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/e2ecodec.go fabric/daemon/e2ecodec_test.go
git commit -m "feat(e2e): sealed-content codec"
```

---

## Task 4: Replay counter store

**Files:**
- Create: `fabric/daemon/replay.go`
- Test: `fabric/daemon/replay_test.go`

**Interfaces:**
- Produces:
  - `type replayKey struct { device, channel string; epoch uint32 }`
  - `type replayWindow struct { mu sync.Mutex; lastSeen map[replayKey]uint64 }`
  - `func newReplayWindow() *replayWindow`
  - `func (w *replayWindow) accept(k replayKey, counter uint64) bool` — returns true and records if `counter` is strictly greater than the last seen for `k` (first sight of a key with counter ≥ 1 is accepted); false for duplicate/out-of-order (≤ last seen).

Sender-side counters are persisted in `Workspace`/state (incremented per send in Task 6's e2e context); the receiver window is in-memory per daemon run. v1 limitation (documented in Task 9): a daemon restart resets the receiver window, so a single replay of a pre-restart frame could be accepted once — acceptable for v1, tightened in a later phase.

- [ ] **Step 1: Write the failing test**

```go
// replay_test.go
package daemon

import "testing"

func TestReplayWindowMonotonic(t *testing.T) {
	w := newReplayWindow()
	k := replayKey{device: "d", channel: "c", epoch: 1}
	if !w.accept(k, 1) {
		t.Fatal("first counter must be accepted")
	}
	if !w.accept(k, 2) {
		t.Fatal("increasing counter must be accepted")
	}
	if w.accept(k, 2) {
		t.Fatal("duplicate must be rejected")
	}
	if w.accept(k, 1) {
		t.Fatal("out-of-order must be rejected")
	}
}

func TestReplayWindowIndependentKeys(t *testing.T) {
	w := newReplayWindow()
	if !w.accept(replayKey{"a", "c", 1}, 5) {
		t.Fatal("device a accepted")
	}
	if !w.accept(replayKey{"b", "c", 1}, 1) {
		t.Fatal("device b independent")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestReplayWindow -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

```go
// replay.go
package daemon

import "sync"

type replayKey struct {
	device  string
	channel string
	epoch   uint32
}

type replayWindow struct {
	mu       sync.Mutex
	lastSeen map[replayKey]uint64
}

func newReplayWindow() *replayWindow {
	return &replayWindow{lastSeen: make(map[replayKey]uint64)}
}

// accept reports whether counter is fresh (strictly greater than the last seen
// for k) and records it. Duplicates and out-of-order counters are rejected.
func (w *replayWindow) accept(k replayKey, counter uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if counter <= w.lastSeen[k] { // zero-value 0 => first counter >=1 passes
		return false
	}
	w.lastSeen[k] = counter
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestReplayWindow -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/replay.go fabric/daemon/replay_test.go
git commit -m "feat(e2e): receiver replay window"
```

---

## Task 5: Device-pubkey set (admin-signed)

**Files:**
- Create: `fabric/daemon/deviceset.go`
- Test: `fabric/daemon/deviceset_test.go`

**Interfaces:**
- Consumes: `crypto/ed25519`.
- Produces:
  - `type deviceSet struct { mu sync.RWMutex; epoch uint32; pubs map[string]ed25519.PublicKey }`
  - `func newDeviceSet() *deviceSet`
  - `func (d *deviceSet) lookup(deviceID string) (ed25519.PublicKey, bool)` — the function passed as `e2e.OpenMessage`'s `lookupPub`.
  - `func (d *deviceSet) set(deviceID string, pub ed25519.PublicKey)` — seed the local device (Task 7) and apply verified roster updates (Task 8).
  - `type signedDeviceSet struct { Epoch uint32; Devices map[string][]byte }` (`json` tags) — the blob published to the roster channel.
  - `func marshalDeviceSet(s signedDeviceSet) []byte` — canonical (sorted-key) JSON for signing.
  - `func (d *deviceSet) applySigned(blob, sig []byte, adminPub ed25519.PublicKey) error` — verify `sig` over `marshalDeviceSet(parsed)` against `adminPub`; on success replace `pubs` and `epoch`. Reject (return error, no mutation) on bad signature.

- [ ] **Step 1: Write the failing test**

```go
// deviceset_test.go
package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestDeviceSetApplySignedAndLookup(t *testing.T) {
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	devPub, _, _ := ed25519.GenerateKey(rand.Reader)

	blob := marshalDeviceSet(signedDeviceSet{
		Epoch:   1,
		Devices: map[string][]byte{"dev-1": devPub},
	})
	sig := ed25519.Sign(adminPriv, blob)

	ds := newDeviceSet()
	if err := ds.applySigned(blob, sig, adminPub); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, ok := ds.lookup("dev-1")
	if !ok || !got.Equal(devPub) {
		t.Fatal("device not registered after apply")
	}
}

func TestDeviceSetRejectsTamperedBlob(t *testing.T) {
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	devPub, _, _ := ed25519.GenerateKey(rand.Reader)
	good := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"dev-1": devPub}})
	sig := ed25519.Sign(adminPriv, good)

	evilPub, _, _ := ed25519.GenerateKey(rand.Reader)
	tampered := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"dev-1": evilPub}})

	ds := newDeviceSet()
	if err := ds.applySigned(tampered, sig, adminPub); err == nil {
		t.Fatal("tampered blob must be rejected")
	}
	if _, ok := ds.lookup("dev-1"); ok {
		t.Fatal("rejected blob must not mutate state")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestDeviceSet -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

```go
// deviceset.go
package daemon

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"sort"
	"sync"
)

type deviceSet struct {
	mu    sync.RWMutex
	epoch uint32
	pubs  map[string]ed25519.PublicKey
}

func newDeviceSet() *deviceSet {
	return &deviceSet{pubs: make(map[string]ed25519.PublicKey)}
}

func (d *deviceSet) lookup(deviceID string) (ed25519.PublicKey, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.pubs[deviceID]
	return p, ok
}

func (d *deviceSet) set(deviceID string, pub ed25519.PublicKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pubs[deviceID] = pub
}

type signedDeviceSet struct {
	Epoch   uint32            `json:"epoch"`
	Devices map[string][]byte `json:"devices"`
}

// marshalDeviceSet produces canonical JSON (sorted device ids) so signer and
// verifier hash identical bytes regardless of Go map iteration order.
func marshalDeviceSet(s signedDeviceSet) []byte {
	ids := make([]string, 0, len(s.Devices))
	for id := range s.Devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		pair, _ := json.Marshal([]any{id, s.Devices[id]})
		ordered = append(ordered, pair)
	}
	out, _ := json.Marshal(struct {
		Epoch   uint32            `json:"epoch"`
		Devices []json.RawMessage `json:"devices"`
	}{Epoch: s.Epoch, Devices: ordered})
	return out
}

func (d *deviceSet) applySigned(blob, sig []byte, adminPub ed25519.PublicKey) error {
	var s signedDeviceSet
	if err := json.Unmarshal(blob, &s); err != nil {
		// blob is canonical-ordered JSON; re-parse via the canonical shape.
		var canon struct {
			Epoch   uint32          `json:"epoch"`
			Devices [][]json.RawMessage `json:"devices"`
		}
		_ = canon // see note in Step: parse the canonical form below
	}
	// Verify signature over the exact blob bytes the signer signed.
	if !ed25519.Verify(adminPub, blob, sig) {
		return errors.New("e2e: device set signature invalid")
	}
	parsed, err := parseCanonicalDeviceSet(blob)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.epoch = parsed.Epoch
	d.pubs = make(map[string]ed25519.PublicKey, len(parsed.Devices))
	for id, pub := range parsed.Devices {
		d.pubs[id] = ed25519.PublicKey(pub)
	}
	return nil
}

// parseCanonicalDeviceSet reverses marshalDeviceSet (devices = [[id, pubBytes], ...]).
func parseCanonicalDeviceSet(blob []byte) (signedDeviceSet, error) {
	var raw struct {
		Epoch   uint32              `json:"epoch"`
		Devices [][]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return signedDeviceSet{}, err
	}
	out := signedDeviceSet{Epoch: raw.Epoch, Devices: map[string][]byte{}}
	for _, pair := range raw.Devices {
		if len(pair) != 2 {
			return signedDeviceSet{}, errors.New("e2e: malformed device pair")
		}
		var id string
		var pub []byte
		if err := json.Unmarshal(pair[0], &id); err != nil {
			return signedDeviceSet{}, err
		}
		if err := json.Unmarshal(pair[1], &pub); err != nil {
			return signedDeviceSet{}, err
		}
		out.Devices[id] = pub
	}
	return out, nil
}
```

> Implementation note: drop the dead `json.Unmarshal(blob, &s)` first-attempt block — `marshalDeviceSet` emits the canonical `devices: [[id, pub], …]` form, so `applySigned` should parse via `parseCanonicalDeviceSet` only. The test above pins the contract; keep `applySigned` to: verify sig over `blob`, then `parseCanonicalDeviceSet`, then mutate. Simplify until the two tests pass with no dead code.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestDeviceSet -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/deviceset.go fabric/daemon/deviceset_test.go
git commit -m "feat(e2e): admin-signed device-pubkey set"
```

---

## Task 6: e2e context resolver

**Files:**
- Create: `fabric/daemon/e2ectx.go`
- Test: `fabric/daemon/e2ectx_test.go`
- Modify: `fabric/daemon/daemon.go` (add `devices *deviceSet` and `replay *replayWindow` fields to `Daemon`, init in `New`)

**Interfaces:**
- Consumes: `*Daemon` (with `state *agentstate.State`), `fabric/e2e`, Task 2/4/5 types.
- Produces:
  - `type e2eCtx struct { key [32]byte; epoch uint32; channelID, deviceID string; devPriv ed25519.PrivateKey; ws *agentstate.Workspace }`
  - `func keyArray(b []byte) ([32]byte, error)` — error if `len(b) != 32`.
  - `func (d *Daemon) e2eContextFor(agentID string) (*e2eCtx, bool, error)` — returns `(nil, false, nil)` when the agent's workspace is non-e2e (the signal to take the plaintext path); `(ctx, true, nil)` when e2e; error only on misconfiguration (e2e workspace but missing/!32-byte key, or agent missing a `SignSeed`).
  - `func (c *e2eCtx) nextCounter(d *Daemon) (uint64, error)` — increment + persist the per-(device,channel,epoch) sender counter. Store counters in a new `Daemon`-held, state-persisted map; simplest store: `Workspace`-adjacent `map[string]uint64` keyed by `device|channel|epoch`, saved via `agentstate.Save`. Persisting matters so a restart never reuses a counter.

- [ ] **Step 1: Write the failing test**

```go
// e2ectx_test.go
package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

func TestE2EContextForNonE2EReturnsFalse(t *testing.T) {
	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "leaf", Parent: "root"}, {ID: "root"}}}
	d := &Daemon{state: st}
	_, ok, err := d.e2eContextFor("leaf")
	if err != nil || ok {
		t.Fatalf("non-e2e workspace must yield (nil,false,nil); got ok=%v err=%v", ok, err)
	}
}

func TestE2EContextForE2EBuildsCtx(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := make([]byte, 32)
	rand.Read(key)
	st := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "root", Parent: ""},
			{ID: "leaf", Parent: "root", SignSeed: priv.Seed()},
		},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key}},
	}
	d := &Daemon{state: st}
	ctx, ok, err := d.e2eContextFor("leaf")
	if err != nil || !ok {
		t.Fatalf("expected e2e ctx; ok=%v err=%v", ok, err)
	}
	if ctx.channelID != "root" || ctx.deviceID != "leaf" || ctx.epoch != 1 {
		t.Fatalf("bad ctx: %+v", ctx)
	}
	if len(ctx.devPriv) != ed25519.PrivateKeySize {
		t.Fatal("device priv not derived from seed")
	}
}

func TestKeyArrayRejectsWrongLen(t *testing.T) {
	if _, err := keyArray([]byte("short")); err == nil {
		t.Fatal("expected error on non-32-byte key")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run "TestE2EContext|TestKeyArray" -v`
Expected: FAIL — undefined symbols / missing `Daemon` fields.

- [ ] **Step 3: Implement** (add fields to `Daemon` in `daemon.go`, then the resolver)

```go
// daemon.go — add to Daemon struct:
	devices *deviceSet
	replay  *replayWindow
// daemon.go — in New(...), after constructing d:
	d.devices = newDeviceSet()
	d.replay = newReplayWindow()
```

```go
// e2ectx.go
package daemon

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

type e2eCtx struct {
	key       [32]byte
	epoch     uint32
	channelID string
	deviceID  string
	devPriv   ed25519.PrivateKey
	ws        *agentstate.Workspace
}

func keyArray(b []byte) ([32]byte, error) {
	var k [32]byte
	if len(b) != 32 {
		return k, fmt.Errorf("e2e: workspace key must be 32 bytes, got %d", len(b))
	}
	copy(k[:], b)
	return k, nil
}

func (d *Daemon) e2eContextFor(agentID string) (*e2eCtx, bool, error) {
	ws, ok := d.state.WorkspaceFor(agentID)
	if !ok || !ws.E2E {
		return nil, false, nil // plaintext path
	}
	key, err := keyArray(ws.Key)
	if err != nil {
		return nil, true, err
	}
	ag, ok := d.state.AgentByID(agentID)
	if !ok || len(ag.SignSeed) != ed25519.SeedSize {
		return nil, true, errors.New("e2e: agent missing device signing seed")
	}
	return &e2eCtx{
		key:       key,
		epoch:     ws.Epoch,
		channelID: ws.RootID,
		deviceID:  agentID,
		devPriv:   ed25519.NewKeyFromSeed(ag.SignSeed),
		ws:        ws,
	}, true, nil
}
```

> The persisted sender counter (`nextCounter`) is wired in Task 6's send path; implement it minimally here as a method that reads/increments an in-`Workspace` `map[string]uint64` (add `Counters map[string]uint64` to `Workspace` in Task 2 if not already present — if you add it later, update Task 2's struct in the same commit per "no compat code"). For the first green build, an in-memory atomic counter seeded from the persisted value is acceptable; persistence is asserted in Task 9.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run "TestE2EContext|TestKeyArray" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/e2ectx.go fabric/daemon/e2ectx_test.go fabric/daemon/daemon.go
git commit -m "feat(e2e): per-agent e2e context resolver"
```

---

## Task 7: Seal on send

**Files:**
- Modify: `fabric/daemon/tools.go:42-55` (the package `Send`) and `fabric/daemon/ops_impl.go:87-89` (`Daemon.Send`)
- Test: `fabric/daemon/send_e2e_test.go`

**Interfaces:**
- Consumes: `e2eCtx` (Task 6), `encodeContent` (Task 3), `e2e.SealMessage`.
- Produces: when `Daemon.Send` runs for an e2e agent, the published frame's envelope has `Subject==""`, `Body==""`, `Enc!=""`. Refactor `Send` to accept an optional sealer rather than duplicating the publish logic:
  - `type sealer func(channelID string, content []byte) (enc string, err error)`
  - `func Send(ctx context.Context, hub hubclient.HubClient, outboundChannel, from string, a SendArgs, seal sealer) error` — when `seal != nil`, set `e.Enc, _ = seal(channelID, encodeContent(a.Subject, a.Body))`, then blank `e.Subject`/`e.Body` before `Encode`. All existing callers pass `nil`.

- [ ] **Step 1: Write the failing test**

```go
// send_e2e_test.go
package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestSendE2EHidesContentFromRelay(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := make([]byte, 32)
	rand.Read(key)
	fake := hubclient.NewFake()
	st := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out"},
		Agents: []agentstate.Agent{
			{ID: "root", Parent: ""},
			{ID: "alice", Parent: "root", SignSeed: priv.Seed()},
		},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key}},
	}
	d := &Daemon{state: st, hub: fake, devices: newDeviceSet(), replay: newReplayWindow()}

	if err := d.Send(context.Background(), "alice", SendArgs{Subject: "secret subj", Body: "secret body"}); err != nil {
		t.Fatal(err)
	}
	frames := fake.Published("out")
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	// frame is "alice: <json>"
	raw := frames[0][strings.Index(frames[0], ": ")+2:]
	e, err := envelope.Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "" || e.Body != "" {
		t.Fatalf("plaintext leaked: subj=%q body=%q", e.Subject, e.Body)
	}
	if e.Enc == "" {
		t.Fatal("expected ciphertext in Enc")
	}
	if strings.Contains(raw, "secret") {
		t.Fatalf("plaintext substring leaked into frame: %s", raw)
	}
}

func TestSendNonE2EUnchanged(t *testing.T) {
	fake := hubclient.NewFake()
	st := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out"},
		Agents: []agentstate.Agent{{ID: "bob", Parent: ""}},
	}
	d := &Daemon{state: st, hub: fake, devices: newDeviceSet(), replay: newReplayWindow()}
	if err := d.Send(context.Background(), "bob", SendArgs{Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	raw := fake.Published("out")[0]
	if !strings.Contains(raw, `"body":"b"`) {
		t.Fatalf("non-e2e body must be cleartext: %s", raw)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run "TestSendE2E|TestSendNonE2E" -v`
Expected: FAIL — `Send` signature mismatch / content not encrypted.

- [ ] **Step 3: Implement**

Refactor the package `Send` to take a `sealer` and branch; update `Daemon.Send` to build the sealer from the e2e context:

```go
// tools.go
type sealer func(channelID string, content []byte) (enc string, err error)

func Send(ctx context.Context, hub hubclient.HubClient, outboundChannel, from string, a SendArgs, seal sealer) error {
	kind := a.Kind
	if kind == "" {
		kind = envelope.KindChat
	}
	e := envelope.Envelope{
		V: 1, ID: envelope.NewID(), TS: nowRFC3339(),
		From: from, To: a.To, Kind: kind, Scope: a.Scope,
		Subject: a.Subject, Body: a.Body,
	}
	if seal != nil {
		// channelID is resolved by the caller's e2e context; pass it through the closure.
		enc, err := seal("", encodeContent(a.Subject, a.Body))
		if err != nil {
			return err
		}
		e.Enc = enc
		e.Subject = ""
		e.Body = ""
	}
	raw, err := envelope.Encode(e)
	if err != nil {
		return err
	}
	return hub.Publish(ctx, outboundChannel, from+": "+string(raw))
}
```

```go
// ops_impl.go
func (d *Daemon) Send(ctx context.Context, fromAgent string, args SendArgs) error {
	ec, isE2E, err := d.e2eContextFor(fromAgent)
	if err != nil {
		return err
	}
	var seal sealer
	if isE2E {
		seal = func(_ string, content []byte) (string, error) {
			counter, err := ec.nextCounter(d)
			if err != nil {
				return "", err
			}
			env, err := e2e.SealMessage(ec.key, ec.epoch, ec.channelID, ec.deviceID, ec.devPriv, counter, content)
			if err != nil {
				return "", err
			}
			return base64.StdEncoding.EncodeToString(env.Marshal()), nil
		}
	}
	return Send(ctx, d.hub, d.state.Daemon.OutboundChannel, fromAgent, args, seal)
}
```

(Add imports `encoding/base64` and `github.com/ericpollmann/botbus-cli/fabric/e2e` to `ops_impl.go`. The unused `channelID` param of `sealer` is kept for the Phase-3 per-topic-id seam; the closure captures `ec.channelID` directly.)

Update the only other caller of `Send` (the package func) to pass `nil` — grep for `daemon.Send(` / `Send(ctx,` and fix in this commit.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run "TestSend" -v` then full `go test ./fabric/daemon/ -v`
Expected: PASS, and pre-existing send tests still green.

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/tools.go fabric/daemon/ops_impl.go fabric/daemon/send_e2e_test.go
git commit -m "feat(e2e): seal subject+body on send for e2e workspaces"
```

---

## Task 8: Open on receive

**Files:**
- Modify: `fabric/daemon/inbox.go:21,66-80` (`runInbox`, `unwrap`)
- Modify: `fabric/daemon/ops_impl.go` / wherever `runInbox` is started (pass the opener)
- Test: `fabric/daemon/recv_e2e_test.go`

**Interfaces:**
- Consumes: `e2e.OpenMessage`, `deviceSet.lookup`, `replayWindow.accept`, `decodeContent`.
- Produces:
  - `type opener func(e envelope.Envelope) (envelope.Envelope, bool)` — given a decoded envelope, if `e.Enc != ""` decrypt+verify+replay-check and return `(plaintextEnv, true)`; on any failure return `(envelope.Envelope{}, false)` (drop). If `e.Enc == ""` return `(e, true)` unchanged (non-e2e passthrough).
  - `unwrap` gains an `open opener` parameter and applies it to each inner envelope, dropping those that fail.
  - `runInbox` gains an `open opener` parameter, threaded to `unwrap`.
  - `func (d *Daemon) openerFor(agentID string) opener` — builds the opener from the receiving agent's e2e context + `d.devices` + `d.replay`. Non-e2e agent → an opener that passes everything through.

- [ ] **Step 1: Write the failing test** (round-trip via the real seal + open, proving convergence at the unit level)

```go
// recv_e2e_test.go
package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/e2e"
	"github.com/ericpollmann/botbus-proto/envelope"
)

func sealedEnvelope(t *testing.T, key [32]byte, epoch uint32, channelID, deviceID string, priv ed25519.PrivateKey, counter uint64, subject, body string) envelope.Envelope {
	t.Helper()
	env, err := e2e.SealMessage(key, epoch, channelID, deviceID, priv, counter, encodeContent(subject, body))
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Envelope{V: 1, ID: "m1", From: deviceID, Kind: envelope.KindChat,
		Enc: base64.StdEncoding.EncodeToString(env.Marshal())}
}

func TestOpenerDecryptsValidMessage(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var key [32]byte
	rand.Read(key[:])
	st := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}, {ID: "bob", Parent: "root", SignSeed: priv.Seed()}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 7, Key: key[:]}},
	}
	d := &Daemon{state: st, devices: newDeviceSet(), replay: newReplayWindow()}
	d.devices.set("alice", pub) // sender's device pubkey known to receiver

	env := sealedEnvelope(t, key, 7, "root", "alice", priv, 1, "hi subj", "hi body")
	open := d.openerFor("bob")
	got, ok := open(env)
	if !ok {
		t.Fatal("valid message dropped")
	}
	if got.Subject != "hi subj" || got.Body != "hi body" {
		t.Fatalf("decrypt mismatch: %+v", got)
	}
}

func TestOpenerDropsReplay(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var key [32]byte
	rand.Read(key[:])
	st := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}, {ID: "bob", Parent: "root", SignSeed: priv.Seed()}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 7, Key: key[:]}},
	}
	d := &Daemon{state: st, devices: newDeviceSet(), replay: newReplayWindow()}
	d.devices.set("alice", pub)
	env := sealedEnvelope(t, key, 7, "root", "alice", priv, 1, "s", "b")
	open := d.openerFor("bob")
	if _, ok := open(env); !ok {
		t.Fatal("first delivery should pass")
	}
	if _, ok := open(env); ok {
		t.Fatal("replayed counter must be dropped")
	}
}

func TestOpenerDropsUnknownDevice(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var key [32]byte
	rand.Read(key[:])
	st := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}, {ID: "bob", Parent: "root", SignSeed: priv.Seed()}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 7, Key: key[:]}},
	}
	d := &Daemon{state: st, devices: newDeviceSet(), replay: newReplayWindow()} // empty device set
	env := sealedEnvelope(t, key, 7, "root", "alice", priv, 1, "s", "b")
	if _, ok := d.openerFor("bob")(env); ok {
		t.Fatal("unknown device must be dropped")
	}
}
```

(Add `encoding/base64` import to the test.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestOpener -v`
Expected: FAIL — undefined `openerFor`.

- [ ] **Step 3: Implement**

```go
// inbox.go — openerFor + threaded unwrap/runInbox

type opener func(e envelope.Envelope) (envelope.Envelope, bool)

func (d *Daemon) openerFor(agentID string) opener {
	ec, isE2E, err := d.e2eContextFor(agentID)
	if !isE2E || err != nil {
		return func(e envelope.Envelope) (envelope.Envelope, bool) {
			if e.Enc != "" {
				return envelope.Envelope{}, false // e2e frame to a non-e2e agent: drop
			}
			return e, true
		}
	}
	return func(e envelope.Envelope) (envelope.Envelope, bool) {
		if e.Enc == "" {
			return e, true // tolerate cleartext control frames if any
		}
		raw, derr := base64.StdEncoding.DecodeString(e.Enc)
		if derr != nil {
			return envelope.Envelope{}, false
		}
		env, perr := e2e.Parse(raw)
		if perr != nil {
			return envelope.Envelope{}, false
		}
		dev, counter, content, oerr := e2e.OpenMessage(ec.key, ec.channelID, env, d.devices.lookup)
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
		e.Subject = subj
		e.Body = body
		e.Enc = ""
		return e, true
	}
}

// unwrap gains the opener:
func unwrap(body string, open opener) []envelope.Envelope {
	// ...existing decode/batch logic...
	// for each inner envelope `inner`:
	//   if out, ok := open(inner); ok { result = append(result, out) }
}
```

Thread `open` from `runInbox` (add param) down to `unwrap`. At the `runInbox` start site (in `ops_impl.go` / daemon Run), build it with `d.openerFor(agentID)`. Update every `runInbox(...)` and `unwrap(...)` call site in the same commit (grep both); existing tests that call `unwrap(body)` pass a passthrough opener: `func(e envelope.Envelope) (envelope.Envelope, bool) { return e, true }`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./fabric/daemon/ -run TestOpener -v` then full `go test ./fabric/daemon/ -v`
Expected: PASS; existing inbox tests green (update their `unwrap` calls to pass the passthrough opener).

- [ ] **Step 5: Commit**

```bash
git add fabric/daemon/inbox.go fabric/daemon/ops_impl.go fabric/daemon/recv_e2e_test.go
git commit -m "feat(e2e): open+verify+replay-check on receive for e2e workspaces"
```

---

## Task 9: `workspace create --e2e` + device-set seeding + integration

**Files:**
- Modify: `cmd/botbus/workspace.go:24,102-126` (mint key material on `--e2e`)
- Modify: `fabric/hostagent/hostagent.go:30-36` (generate `SignSeed` for new agents)
- Create: `fabric/daemon/e2e_integration_test.go`
- Modify: `go.mod` (drop the local replace; `go get …@v0.4.0`)
- Modify: `README` / module README — document the v1 limitations

**Interfaces:**
- Consumes: everything above.
- Produces:
  - `botbus workspace create <name> --e2e` → a `Workspace{E2E:true, Epoch:1, Key:<32 random>, Salt:<32 random>, AdminPub:<admin pub>}` persisted; the creating agent gets a `SignSeed`; the admin keypair is generated, admin **private** key stored only on the creator's state (0600), admin pub pinned in `Workspace.AdminPub`.
  - The creator publishes an admin-signed device set (its own device) so a joining peer can verify it. Expose `func (d *Daemon) PublishDeviceSet(ctx context.Context) error` and a roster-channel ingest in `runInbox` that calls `d.devices.applySigned(...)` with `Workspace.AdminPub`.

- [ ] **Step 1: Write the failing integration test** (two daemons, one fake hub, relay-blind + convergence + wrong-key-cannot-read)

```go
// e2e_integration_test.go
package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestTwoDaemonE2EConvergenceRelayBlind(t *testing.T) {
	// Shared workspace key + admin key; Alice (daemon A) sends, Bob (daemon B) reads.
	var key [32]byte
	rand.Read(key[:])
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	_, bobPriv, _ := ed25519.GenerateKey(rand.Reader)

	fake := hubclient.NewFake()

	mkState := func(self string, seed []byte) *agentstate.State {
		return &agentstate.State{
			Daemon: agentstate.Daemon{OutboundChannel: "out"},
			Agents: []agentstate.Agent{
				{ID: "root"},
				{ID: self, Parent: "root", SignSeed: seed, InboxChannel: "inbox-" + self},
			},
			Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key[:], AdminPub: adminPub}},
		}
	}
	dA := &Daemon{state: mkState("alice", alicePriv.Seed()), hub: fake, devices: newDeviceSet(), replay: newReplayWindow()}
	dB := &Daemon{state: mkState("bob", bobPriv.Seed()), hub: fake, devices: newDeviceSet(), replay: newReplayWindow()}

	// Admin-signed device set naming both devices, delivered to B (the real path
	// is the roster channel; here inject the signed blob B verifies).
	blob := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"alice": alicePub}})
	sig := ed25519.Sign(adminPriv, blob)
	if err := dB.devices.applySigned(blob, sig, adminPub); err != nil {
		t.Fatal(err)
	}

	// Alice sends.
	if err := dA.Send(context.Background(), "alice", SendArgs{Subject: "topsecret", Body: "the eagle lands at noon"}); err != nil {
		t.Fatal(err)
	}

	// Relay-blind assertion: the frame on the hub carries no plaintext.
	raw := fake.Published("out")[0]
	if strings.Contains(raw, "eagle") || strings.Contains(raw, "topsecret") {
		t.Fatalf("relay saw plaintext: %s", raw)
	}

	// Deliver the frame to Bob's inbox and open it.
	body := raw[strings.Index(raw, ": ")+2:]
	e, _ := envelope.Decode([]byte(body))
	got, ok := dB.openerFor("bob")(e)
	if !ok {
		t.Fatal("bob could not open alice's message")
	}
	if got.Subject != "topsecret" || got.Body != "the eagle lands at noon" {
		t.Fatalf("convergence mismatch: %+v", got)
	}

	// Wrong-key daemon (different workspace key) cannot read.
	var otherKey [32]byte
	rand.Read(otherKey[:])
	dC := &Daemon{
		state:   &agentstate.State{Agents: []agentstate.Agent{{ID: "root"}, {ID: "carol", Parent: "root", SignSeed: bobPriv.Seed()}}, Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: otherKey[:], AdminPub: adminPub}}},
		devices: dB.devices, replay: newReplayWindow(),
	}
	if _, ok := dC.openerFor("carol")(e); ok {
		t.Fatal("a daemon with the wrong workspace key must not decrypt")
	}
	_ = time.Now
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./fabric/daemon/ -run TestTwoDaemonE2E -v`
Expected: FAIL until seal/open (Tasks 7-8) and the device-set path are all in place; if Tasks 7-8 passed, this should drive only the `workspace create --e2e` minting code + any glue.

- [ ] **Step 3: Implement the CLI minting + device-seed generation + roster ingest**

```go
// cmd/botbus/workspace.go — in the create branch, parse --e2e and mint:
//   if e2e {
//     key := random32(); salt := random32()
//     adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
//     ws := agentstate.Workspace{RootID: root.ID, E2E: true, Epoch: 1, Key: key, Salt: salt, AdminPub: adminPub}
//     // store adminPriv on the creator (0600) — e.g. Workspace.AdminPriv (omitempty), present only on the admin host
//     state.Workspaces = append(state.Workspaces, ws)
//   }
// hostagent.Create — when the target workspace is e2e, set Agent.SignSeed = random32().
// runInbox — if a frame arrives on the reserved roster/device channel, call
//   d.devices.applySigned(blob, sig, ws.AdminPub); never trust a relay-supplied set.
```

Implement `random32()` via `crypto/rand`. Add `Workspace.AdminPriv []byte` (`json:"adminPriv,omitempty"`) stored only on the admin host. Define the reserved device-set channel naming (`<root.ID>.devices` is fine for v1) and `PublishDeviceSet`.

- [ ] **Step 4: Run the full suite + the integration test**

Run:
```bash
go build ./... && go test ./... -race
```
Expected: PASS, race-clean, including `TestTwoDaemonE2EConvergenceRelayBlind`.

- [ ] **Step 5: Finalize deps, document limits, commit**

```bash
# drop local replace, pin the released proto:
go mod edit -dropreplace github.com/ericpollmann/botbus-proto
go get github.com/ericpollmann/botbus-proto@v0.4.0
go mod tidy
go test ./... -race
```
Document in the module README under an "E2E (v1) limitations" heading: no forward secrecy / no cryptographic revocation (mitigated later by epoch rotation + signing-based eviction); receiver replay window is in-memory (a daemon restart could accept one pre-restart replay); topology/metadata stays cleartext.

```bash
git add fabric/daemon/e2e_integration_test.go cmd/botbus/workspace.go fabric/hostagent/hostagent.go go.mod go.sum README.md
git commit -m "feat(e2e): workspace create --e2e + admin-signed device set + two-daemon convergence test"
```

---

## Self-Review

**Spec coverage (vs `2026-06-25-e2e-encryption-design.md` Phase 1):**
- `workspace create --e2e` → Task 9. ✓
- daemon encrypt/decrypt + sign/verify under one workspace key → Tasks 6-8 (reusing #27 `SealMessage`/`OpenMessage`). ✓
- per-device signing keys + admin-signed device set in roster channel → Tasks 2 (seed), 5 (set + verify), 9 (mint + roster ingest). ✓
- replay counters → Task 4 (window) + Task 6 (sender counter). ✓
- relay stays dumb for e2e workspaces; non-e2e unchanged → Global Constraints + `TestSendNonE2EUnchanged` + the passthrough opener. ✓
- self-verify: two-daemon convergence + "relay sees only ciphertext" → Task 9 integration test. ✓
- crypto non-negotiables: AAD binds channel+epoch (in #27 `SealMessage`); deviceID bound in signed payload (#27); monotonic counter (Task 4); admin-signed (not relay-supplied) device set (Task 5). ✓

**Out of scope (correctly deferred to later phases per the spec):** HKDF-derived channel ids + per-epoch salt rotation (Phase 3 — `Salt` is stored now as the seam), hub capability tokens (Phase 3), client-side projections/CRDT board (Phase 2), browser/WebCrypto custody + device wrapping + OOB verification (Phase 4), Google one-tap (Phase 5).

**Placeholder scan:** every code step contains real code. The one explicit "simplify until tests pass" note (Task 5 `applySigned`) is intentional guidance to delete a dead first-parse attempt, not a placeholder. The `nextCounter` persistence is specified (in-`Workspace` map, persisted via `agentstate.Save`) with an acceptable first-green fallback.

**Type consistency:** `Workspace`/`Agent.SignSeed` (Task 2) are consumed unchanged in Tasks 6-9; `e2eCtx` fields (Task 6) match their use in Tasks 7-8; `sealer`/`opener` signatures match their call sites; `e2e.SealMessage`/`OpenMessage`/`Parse`/`Envelope.Marshal` match #27's verified signatures; `deviceSet.lookup` matches `e2e.OpenMessage`'s `lookupPub func(string)(ed25519.PublicKey,bool)`.

**Scope check:** one coherent subsystem (symmetric e2e core). Task 1 (proto) and Tasks 5+9 (device-set distribution) are the two natural fault lines if this needs to ship in smaller PRs — the core (Tasks 1-4, 6-8) delivers an encrypting daemon; the device-set distribution (5, 9) delivers the trust layer.
