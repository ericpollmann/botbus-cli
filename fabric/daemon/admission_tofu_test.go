package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

// TestProcessAdmitGrantTOFU verifies that ProcessAdmitGrant in TOFU mode
// (expectedAdminPub == nil) accepts a validly-signed grant and pins the admin
// pub from the grant itself; a grant with a corrupted Sig is rejected even
// under TOFU.
func TestProcessAdmitGrantTOFU(t *testing.T) {
	// Build a valid grant signed by a test admin key.
	var wsKey [32]byte
	if _, err := rand.Read(wsKey[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	adminPub, adminPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	encPub, encPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("box.GenerateKey: %v", err)
	}
	wrapped, err := wrapKey(wsKey, *encPub)
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	grant := AdmitGrant{
		ReqID:       "req-tofu",
		AnchorID:    "req-tofu",
		RootID:      "root-1",
		Epoch:       1,
		WrappedKey:  wrapped,
		AdminPub:    []byte(adminPub),
		Roster:      "roster",
		WaitingRoom: "wroom",
	}
	grant.Sig = ed25519.Sign(adminPriv, grantSignedPayload(grant))

	// (a) Valid grant, TOFU (nil expectedAdminPub) — must succeed and pin AdminPub.
	ws, key, ok := ProcessAdmitGrant(grant, encPriv[:], nil)
	if !ok {
		t.Fatal("ProcessAdmitGrant TOFU: expected ok, got false")
	}
	if key != wsKey {
		t.Fatalf("TOFU: key mismatch: got %x want %x", key, wsKey)
	}
	if !bytes.Equal(ws.AdminPub, []byte(adminPub)) {
		t.Fatalf("TOFU: AdminPub not pinned: got %x want %x", ws.AdminPub, []byte(adminPub))
	}

	// (b) Corrupt the signature — must be rejected even in TOFU mode.
	badGrant := grant
	badGrant.Sig = append([]byte(nil), grant.Sig...)
	badGrant.Sig[0] ^= 0xFF
	if _, _, ok := ProcessAdmitGrant(badGrant, encPriv[:], nil); ok {
		t.Fatal("corrupted Sig must be rejected in TOFU mode")
	}
}
