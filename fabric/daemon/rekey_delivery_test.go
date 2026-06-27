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
	d, fake, ws := newAdminDaemon(t)
	ctx := context.Background()

	encPub, encPriv, _ := box.GenerateKey(rand.Reader)
	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, _ = d.AdmitJoinRequest(ctx, ws, JoinRequest{ReqID: "j1", SignPub: signPub, EncPub: encPub[:]})

	before := len(fake.Published("roster"))
	if _, err := d.RotateKey(ctx, ws); err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// Pull the rekey grant for j1 out of Published("roster"), same as TestRotatePublishesOpenableSignedRekey.
	var g AdmitGrant
	var found bool
	for _, f := range fake.Published("roster")[before:] {
		if len(f) == 0 || f[0] != '{' {
			continue
		}
		parsed, perr := parseAdmitGrant([]byte(f))
		if perr != nil || parsed.AnchorID != "j1" {
			continue
		}
		g = parsed
		found = true
		break
	}
	if !found {
		t.Fatal("no signed rekey grant for j1 found — cannot test wrong-admin rejection")
	}

	wrongAdmin, _, _ := ed25519.GenerateKey(rand.Reader)
	// A grant verified against the wrong admin pubkey must be rejected.
	if _, _, ok := ProcessRekey(g, encPriv[:], wrongAdmin); ok {
		t.Fatal("ProcessRekey accepted a grant under the wrong admin pubkey")
	}
}
