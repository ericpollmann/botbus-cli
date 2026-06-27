package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestSignVerifyCert(t *testing.T) {
	parentPub, parentPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	childPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	c := SignCert(parentPriv, "child-device-1", "parent-device-0", childPub)

	if c.ChildID != "child-device-1" {
		t.Errorf("ChildID = %q, want %q", c.ChildID, "child-device-1")
	}
	if c.ParentID != "parent-device-0" {
		t.Errorf("ParentID = %q, want %q", c.ParentID, "parent-device-0")
	}
	if len(c.ChildSignPub) != ed25519.PublicKeySize {
		t.Errorf("ChildSignPub len = %d, want %d", len(c.ChildSignPub), ed25519.PublicKeySize)
	}
	if len(c.Sig) != ed25519.SignatureSize {
		t.Errorf("Sig len = %d, want %d", len(c.Sig), ed25519.SignatureSize)
	}

	if !VerifyCert(c, parentPub) {
		t.Error("round-trip: VerifyCert returned false, want true")
	}
}

func TestVerifyCertRejectsTamper(t *testing.T) {
	parentPub, parentPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	childPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	c := SignCert(parentPriv, "child-1", "parent-0", childPub)

	t.Run("wrong parent pubkey", func(t *testing.T) {
		if VerifyCert(c, otherPub) {
			t.Error("expected false with wrong parent pubkey")
		}
	})

	t.Run("tampered ChildID", func(t *testing.T) {
		bad := c
		bad.ChildID = "tampered-child"
		if VerifyCert(bad, parentPub) {
			t.Error("expected false with tampered ChildID")
		}
	})

	t.Run("tampered ChildSignPub", func(t *testing.T) {
		bad := c
		bad.ChildSignPub = make([]byte, ed25519.PublicKeySize)
		copy(bad.ChildSignPub, c.ChildSignPub)
		bad.ChildSignPub[0] ^= 0xFF
		if VerifyCert(bad, parentPub) {
			t.Error("expected false with tampered ChildSignPub")
		}
	})

	t.Run("tampered Sig", func(t *testing.T) {
		bad := c
		bad.Sig = make([]byte, ed25519.SignatureSize)
		copy(bad.Sig, c.Sig)
		bad.Sig[0] ^= 0xFF
		if VerifyCert(bad, parentPub) {
			t.Error("expected false with tampered Sig")
		}
	})
}

func TestVerifyCertDomainSeparation(t *testing.T) {
	parentPub, parentPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	childPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	c := SignCert(parentPriv, "child-1", "parent-0", childPub)

	// Replace Sig with a signature over different bytes (no domain tag).
	otherPayload := []byte("some other protocol bytes")
	wrongSig := ed25519.Sign(parentPriv, otherPayload)
	bad := c
	bad.Sig = wrongSig

	if VerifyCert(bad, parentPub) {
		t.Error("domain separation: sig over different bytes should not verify as cert")
	}
}
