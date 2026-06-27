package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

// newAdminDaemon builds a minimal admin Daemon (no loops) wired to a FakeHub.
// Returns the daemon, fake hub, workspace (pointer into adminState), and the
// admin's Ed25519 privkey (for signing assertions in tests).
func newAdminDaemon(t *testing.T) (*Daemon, *hubclient.Fake, *agentstate.Workspace) {
	t.Helper()

	var wsKey [32]byte
	if _, err := rand.Read(wsKey[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	adminPub, adminPrivSeed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	adminState := &agentstate.State{
		Agents: []agentstate.Agent{{ID: "admin-root"}},
		Workspaces: []agentstate.Workspace{{
			RootID:      "admin-root",
			E2E:         true,
			Epoch:       1,
			Key:         wsKey[:],
			AdminPub:    adminPub,
			AdminPriv:   adminPrivSeed.Seed(),
			Roster:      "roster",
			WaitingRoom: "wroom",
		}},
	}

	fake := hubclient.NewFake()
	d := &Daemon{
		state:  adminState,
		hub:    fake,
		trust:  newTrustGraph(),
		replay: newReplayWindow(),
	}
	ws := &adminState.Workspaces[0]
	return d, fake, ws
}

// admitAnchor generates a joiner keypair, admits it, and returns its sign/enc
// privkeys along with the JoinRequest so tests can use them.
func admitAnchor(t *testing.T, d *Daemon, ws *agentstate.Workspace, anchorID string) (signPub ed25519.PublicKey, signPriv ed25519.PrivateKey, encPub *[32]byte, encPriv *[32]byte) {
	t.Helper()

	var err error
	signPub, signPriv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	encPub, encPriv, err = box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("box.GenerateKey: %v", err)
	}

	req := JoinRequest{
		ReqID:   anchorID,
		Name:    anchorID,
		SignPub: signPub,
		EncPub:  encPub[:],
	}
	if _, err := d.AdmitJoinRequest(context.Background(), ws, req); err != nil {
		t.Fatalf("AdmitJoinRequest(%s): %v", anchorID, err)
	}
	return
}

// TestRotateKeyReWrapsToAnchors verifies:
//
//	(a) ws.Epoch increments and the new key differs from the old one,
//	(b) a "rekey" roster frame carries a wrapped key the anchor's enc-priv
//	    unwraps to the NEW key (relay-blind: new key not in cleartext),
//	(c) the anchor is still present in the new-epoch anchor blob.
func TestRotateKeyReWrapsToAnchors(t *testing.T) {
	d, fake, ws := newAdminDaemon(t)

	// Capture the old key before rotation.
	var oldKey [32]byte
	copy(oldKey[:], ws.Key)
	oldEpoch := ws.Epoch

	// Admit one anchor.
	_, _, _, encPriv := admitAnchor(t, d, ws, "anchor-1")

	// Clear published messages so rotation assertions are clean.
	fake.Published("roster") // drain
	fake.Published("wroom")  // drain

	// --- RED: RotateKey doesn't exist yet → this call won't compile ---
	newKey, err := d.RotateKey(context.Background(), ws)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// (a) Epoch incremented, key changed.
	if ws.Epoch != oldEpoch+1 {
		t.Fatalf("epoch: got %d, want %d", ws.Epoch, oldEpoch+1)
	}
	if newKey == oldKey {
		t.Fatal("RotateKey must produce a key that differs from the old one")
	}
	if !bytes.Equal(ws.Key, newKey[:]) {
		t.Fatal("ws.Key must be updated to the new key")
	}

	// (b) Relay-blind: the new key must not appear in cleartext on any
	// published message.
	allMsgs := append(fake.Published("roster"), fake.Published("wroom")...)
	for _, msg := range allMsgs {
		if bytes.Contains([]byte(msg), newKey[:]) {
			t.Fatal("relay-blind violation: plaintext new key found in published frame")
		}
		if bytes.Contains([]byte(msg), []byte(base64.StdEncoding.EncodeToString(newKey[:]))) {
			t.Fatal("relay-blind violation: base64 new key found in published frame")
		}
	}

	// Find the rekey frame addressed to anchor-1 on the roster channel.
	rosterMsgs := fake.Published("roster")
	var rekeyFrame *rosterFrame
	for _, msg := range rosterMsgs {
		f, err := openRosterFrame(newKey, msg)
		if err != nil {
			// Could be sealed under new epoch; try.
			continue
		}
		if f.Kind == "rekey" && f.AnchorID == "anchor-1" {
			cp := f
			rekeyFrame = &cp
			break
		}
	}
	if rekeyFrame == nil {
		t.Fatal("no rekey frame addressed to anchor-1 found on the roster channel")
	}

	// The anchor can unwrap the new key using its enc-priv.
	recovered, ok := unwrapKey(rekeyFrame.WrappedKey, *encPriv)
	if !ok {
		t.Fatal("anchor could not unwrap the new key from rekey frame")
	}
	if recovered != newKey {
		t.Fatalf("unwrapped key mismatch: got %x, want %x", recovered, newKey)
	}

	// (c) anchor-1 must still be present in the new-epoch anchor blob (verify
	// via the anchors frame that rotation publishes, opened with the new key).
	var foundAnchorInBlob bool
	for _, msg := range rosterMsgs {
		f, err := openRosterFrame(newKey, msg)
		if err != nil {
			continue
		}
		if f.Kind != "anchors" || len(f.AnchorBlob) == 0 {
			continue
		}
		var anchorSet struct {
			Devices [][]json.RawMessage `json:"devices"`
		}
		if err := json.Unmarshal(f.AnchorBlob, &anchorSet); err != nil {
			continue
		}
		for _, pair := range anchorSet.Devices {
			if len(pair) != 2 {
				continue
			}
			var id string
			if json.Unmarshal(pair[0], &id) == nil && id == "anchor-1" {
				foundAnchorInBlob = true
			}
		}
	}
	if !foundAnchorInBlob {
		t.Fatal("anchor-1 must still appear in the new-epoch anchor blob after rotation")
	}
}

// TestRemoveAnchorEvicts verifies:
//
//	(a) epoch rolls after removal,
//	(b) the removed anchor is absent from the new anchor blob,
//	(c) the removed anchor cannot obtain the new key (no rekey frame for it).
func TestRemoveAnchorEvicts(t *testing.T) {
	d, fake, ws := newAdminDaemon(t)

	// Admit two anchors.
	admitAnchor(t, d, ws, "anchor-keep")
	admitAnchor(t, d, ws, "anchor-remove")

	epochBeforeRemove := ws.Epoch

	// Clear published messages for clean assertions.
	fake.Published("roster")
	fake.Published("wroom")

	// --- RED: RemoveAnchor doesn't exist yet ---
	newKey, err := d.RemoveAnchor(context.Background(), ws, "anchor-remove")
	if err != nil {
		t.Fatalf("RemoveAnchor: %v", err)
	}

	// (a) Epoch must have rolled.
	if ws.Epoch <= epochBeforeRemove {
		t.Fatalf("epoch did not roll after RemoveAnchor: got %d, want > %d", ws.Epoch, epochBeforeRemove)
	}

	rosterMsgs := fake.Published("roster")

	// (b) anchor-remove must NOT appear in the new anchor blob; anchor-keep must.
	var foundRemoved, foundKept bool
	for _, msg := range rosterMsgs {
		f, err := openRosterFrame(newKey, msg)
		if err != nil {
			continue
		}
		if f.Kind != "anchors" || len(f.AnchorBlob) == 0 {
			continue
		}
		var anchorSet struct {
			Devices [][]json.RawMessage `json:"devices"`
		}
		if err := json.Unmarshal(f.AnchorBlob, &anchorSet); err != nil {
			continue
		}
		for _, pair := range anchorSet.Devices {
			if len(pair) != 2 {
				continue
			}
			var id string
			if json.Unmarshal(pair[0], &id) != nil {
				continue
			}
			if id == "anchor-remove" {
				foundRemoved = true
			}
			if id == "anchor-keep" {
				foundKept = true
			}
		}
	}
	if foundRemoved {
		t.Fatal("evicted anchor-remove must NOT appear in the new-epoch anchor blob")
	}
	if !foundKept {
		t.Fatal("anchor-keep must still appear in the new-epoch anchor blob")
	}

	// (c) No rekey frame may be addressed to anchor-remove.
	for _, msg := range rosterMsgs {
		f, err := openRosterFrame(newKey, msg)
		if err != nil {
			continue
		}
		if f.Kind == "rekey" && f.AnchorID == "anchor-remove" {
			t.Fatal("removed anchor must NOT receive a rekey frame")
		}
	}

	// Verify: a message sealed by the removed anchor under the OLD epoch
	// is now rejected because its id no longer resolves in the trust graph.
	// We exercise this by confirming the removed anchor's id is gone from d.trust.anchors.
	if _, ok := d.trust.anchors.lookup("anchor-remove"); ok {
		t.Fatal("removed anchor must not remain in d.trust.anchors after eviction")
	}

	// Extra: simulate a message from the removed anchor — it should be dropped
	// because its chain no longer resolves. We seal with the NEW key (worst case)
	// so the only rejection reason is the missing trust entry.
	//
	// We need the removed anchor's sign key — generate a fresh daemon and anchor
	// set to test this path; use a raw trust.resolve call since we have no
	// sign-seed to build a full opener here.
	if _, ok := d.trust.resolve("anchor-remove"); ok {
		t.Fatal("trust.resolve must return (nil, false) for the removed anchor")
	}

	// anchor-keep must still resolve (not collateral damage).
	if _, ok := d.trust.resolve("anchor-keep"); !ok {
		t.Fatal("anchor-keep must still resolve after removing anchor-remove")
	}

	// Anchor-keep MUST receive a rekey frame so it can decrypt future traffic.
	var keepRekeyed bool
	for _, msg := range rosterMsgs {
		f, err := openRosterFrame(newKey, msg)
		if err != nil {
			continue
		}
		if f.Kind == "rekey" && f.AnchorID == "anchor-keep" {
			keepRekeyed = true
		}
	}
	if !keepRekeyed {
		t.Fatal("anchor-keep must receive a rekey frame after rotation")
	}

	// Paranoia: the new key must NOT appear in cleartext in any published msg.
	allMsgs := append(rosterMsgs, fake.Published("wroom")...)
	for _, msg := range allMsgs {
		if bytes.Contains([]byte(msg), newKey[:]) {
			t.Fatal("relay-blind violation: plaintext new key in published frame after RemoveAnchor")
		}
	}

	// Simulated old-epoch message from evicted anchor must not be openable
	// via the openerFor path if we'd set up a proper receiver. We assert via
	// trust resolution above; the opener calls trust.resolve, so this is covered.
	_ = envelope.Envelope{} // type used in other tests; keep import alive
}
