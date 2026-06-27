package daemon

import (
	"bytes"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

// wrapKey → unwrapKey round-trips the 32-byte workspace key.
func TestWrapUnwrapRoundTrip(t *testing.T) {
	encPub, encPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	var workspaceKey [32]byte
	if _, err := rand.Read(workspaceKey[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	blob, err := wrapKey(workspaceKey, *encPub)
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	got, ok := unwrapKey(blob, *encPriv)
	if !ok {
		t.Fatal("unwrapKey returned false")
	}
	if got != workspaceKey {
		t.Fatal("unwrapped key does not match original")
	}
}

// Unwrapping with the wrong encPriv returns (_, false).
func TestUnwrapWrongKeyFails(t *testing.T) {
	encPub, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	var workspaceKey [32]byte
	rand.Read(workspaceKey[:]) //nolint:errcheck
	blob, err := wrapKey(workspaceKey, *encPub)
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	_, wrongPriv, _ := box.GenerateKey(rand.Reader)
	_, ok := unwrapKey(blob, *wrongPriv)
	if ok {
		t.Fatal("unwrapKey with wrong key should return false")
	}
}

// JoinRequest JSON round-trip.
func TestJoinRequestRoundTrip(t *testing.T) {
	r := JoinRequest{
		ReqID:        "req-123",
		Name:         "alice",
		ParentIntent: "subtree",
		SignPub:      []byte{1, 2, 3, 4},
		EncPub:       []byte{5, 6, 7, 8},
	}
	b, err := r.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := parseJoinRequest(b)
	if err != nil {
		t.Fatalf("parseJoinRequest: %v", err)
	}
	if got.ReqID != r.ReqID || got.Name != r.Name || got.ParentIntent != r.ParentIntent {
		t.Fatalf("string fields mismatch: %+v", got)
	}
	if !bytes.Equal(got.SignPub, r.SignPub) || !bytes.Equal(got.EncPub, r.EncPub) {
		t.Fatalf("byte slice fields mismatch: %+v", got)
	}
}

// AdmitGrant JSON round-trip.
func TestAdmitGrantRoundTrip(t *testing.T) {
	g := AdmitGrant{
		ReqID:       "req-123",
		AnchorID:    "anchor-1",
		RootID:      "root-1",
		Epoch:       2,
		WrappedKey:  []byte{10, 20, 30},
		AdminPub:    []byte{40, 50, 60},
		Roster:      "roster-chan",
		WaitingRoom: "wait-chan",
	}
	b, err := g.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := parseAdmitGrant(b)
	if err != nil {
		t.Fatalf("parseAdmitGrant: %v", err)
	}
	if got.ReqID != g.ReqID || got.AnchorID != g.AnchorID || got.RootID != g.RootID || got.Epoch != g.Epoch {
		t.Fatalf("fields mismatch: %+v", got)
	}
	if !bytes.Equal(got.WrappedKey, g.WrappedKey) || !bytes.Equal(got.AdminPub, g.AdminPub) {
		t.Fatalf("byte fields mismatch: %+v", got)
	}
	if got.Roster != g.Roster || got.WaitingRoom != g.WaitingRoom {
		t.Fatalf("channel fields mismatch: %+v", got)
	}
}

// sasFingerprint is deterministic (same input → same output).
func TestSasFingerprintDeterministic(t *testing.T) {
	signPub := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	encPub := []byte{9, 10, 11, 12, 13, 14, 15, 16}
	fp1 := sasFingerprint(signPub, encPub)
	fp2 := sasFingerprint(signPub, encPub)
	if fp1 != fp2 {
		t.Fatalf("sasFingerprint not deterministic: %q != %q", fp1, fp2)
	}
	// Must look like XXXX-XXXX-XXXX (two dashes, 14 chars total).
	if len(fp1) != 14 {
		t.Fatalf("sasFingerprint length = %d, want 14: %q", len(fp1), fp1)
	}
	if fp1[4] != '-' || fp1[9] != '-' {
		t.Fatalf("sasFingerprint format wrong: %q", fp1)
	}
}

// sasFingerprint differs for different inputs.
func TestSasFingerprintDiffers(t *testing.T) {
	signPub1 := []byte{1, 2, 3, 4}
	signPub2 := []byte{5, 6, 7, 8}
	encPub := []byte{9, 10, 11, 12}
	fp1 := sasFingerprint(signPub1, encPub)
	fp2 := sasFingerprint(signPub2, encPub)
	if fp1 == fp2 {
		t.Fatalf("sasFingerprint should differ for different inputs: %q == %q", fp1, fp2)
	}
}
