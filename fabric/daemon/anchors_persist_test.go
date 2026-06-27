package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

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
	newKey, err := fresh.RotateKey(ctx, ws)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	// RotateKey must have published a sealed rekey frame targeting joiner-1,
	// whose wrapped key is decryptable by encPriv.
	found := false
	for _, f := range fake.Published("roster")[rosterBefore:] {
		rf, err := openRosterFrame(newKey, f)
		if err != nil || rf.Kind != "rekey" || rf.AnchorID != "joiner-1" {
			continue
		}
		if _, ok := unwrapKey(rf.WrappedKey, *encPriv); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("fresh RotateKey did not re-wrap to the persisted anchor")
	}
}
