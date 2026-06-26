// deviceset_test.go
package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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

// buildSignedBlob constructs a canonical device-set blob (matching marshalDeviceSet output)
// from ordered pairs, then signs it with adminPriv. This lets tests inject duplicate ids or
// short pubkeys that the Go-map-based marshalDeviceSet cannot represent.
func buildSignedBlob(t *testing.T, epoch uint32, pairs [][]any, adminPriv ed25519.PrivateKey) (blob, sig []byte) {
	t.Helper()
	ordered := make([]json.RawMessage, 0, len(pairs))
	for _, p := range pairs {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("buildSignedBlob marshal pair: %v", err)
		}
		ordered = append(ordered, raw)
	}
	blob, err := json.Marshal(struct {
		Epoch   uint32            `json:"epoch"`
		Devices []json.RawMessage `json:"devices"`
	}{Epoch: epoch, Devices: ordered})
	if err != nil {
		t.Fatalf("buildSignedBlob marshal outer: %v", err)
	}
	return blob, ed25519.Sign(adminPriv, blob)
}

func TestDeviceSetRejectsShortPubkey(t *testing.T) {
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	shortKey := []byte{0x01, 0x02, 0x03} // only 3 bytes, not 32

	blob, sig := buildSignedBlob(t, 1, [][]any{{"dev-bad", shortKey}}, adminPriv)

	ds := newDeviceSet()
	err := ds.applySigned(blob, sig, adminPub)
	if err == nil {
		t.Fatal("expected error for short pubkey, got nil")
	}
	if _, ok := ds.lookup("dev-bad"); ok {
		t.Fatal("set must be unmutated after short-pubkey rejection")
	}
}

func TestDeviceSetRejectsDuplicateDeviceID(t *testing.T) {
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	devPub, _, _ := ed25519.GenerateKey(rand.Reader)
	devPub2, _, _ := ed25519.GenerateKey(rand.Reader)

	// Two entries with the same id "dev-1" — impossible via marshalDeviceSet (map dedupes)
	blob, sig := buildSignedBlob(t, 1, [][]any{
		{"dev-1", []byte(devPub)},
		{"dev-1", []byte(devPub2)},
	}, adminPriv)

	ds := newDeviceSet()
	err := ds.applySigned(blob, sig, adminPub)
	if err == nil {
		t.Fatal("expected error for duplicate device id, got nil")
	}
}
