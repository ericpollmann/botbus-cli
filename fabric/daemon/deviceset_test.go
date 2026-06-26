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
