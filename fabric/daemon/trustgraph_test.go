package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/e2e"
)

// buildAnchorSet signs a single device into the trustGraph's anchor set.
func buildAnchorSet(t *testing.T, g *trustGraph, adminPriv ed25519.PrivateKey, id string, pub ed25519.PublicKey) {
	t.Helper()
	blob := marshalDeviceSet(signedDeviceSet{
		Epoch:   1,
		Devices: map[string][]byte{id: pub},
	})
	sig := ed25519.Sign(adminPriv, blob)
	if err := g.applyAnchorSet(blob, sig, adminPriv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("applyAnchorSet: %v", err)
	}
}

func TestTrustGraphDirectAnchor(t *testing.T) {
	_, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	rootPub, _, _ := ed25519.GenerateKey(rand.Reader)

	g := newTrustGraph()
	buildAnchorSet(t, g, adminPriv, "root", rootPub)

	got, ok := g.resolve("root")
	if !ok {
		t.Fatal("expected anchor to resolve")
	}
	if !got.Equal(rootPub) {
		t.Fatal("resolved pub does not match anchor pub")
	}
}

func TestTrustGraphOneHopChild(t *testing.T) {
	_, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	childPub, _, _ := ed25519.GenerateKey(rand.Reader)

	g := newTrustGraph()
	buildAnchorSet(t, g, adminPriv, "root", rootPub)
	g.addCert(e2e.SignCert(rootPriv, "child", "root", childPub))

	got, ok := g.resolve("child")
	if !ok {
		t.Fatal("expected one-hop child to resolve")
	}
	if !got.Equal(childPub) {
		t.Fatal("resolved pub does not match child pub")
	}
}

func TestTrustGraphTwoHopGrandchild(t *testing.T) {
	_, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	midPub, midPriv, _ := ed25519.GenerateKey(rand.Reader)
	leafPub, _, _ := ed25519.GenerateKey(rand.Reader)

	g := newTrustGraph()
	buildAnchorSet(t, g, adminPriv, "root", rootPub)
	g.addCert(e2e.SignCert(rootPriv, "mid", "root", midPub))
	g.addCert(e2e.SignCert(midPriv, "leaf", "mid", leafPub))

	got, ok := g.resolve("leaf")
	if !ok {
		t.Fatal("expected grandchild to resolve")
	}
	if !got.Equal(leafPub) {
		t.Fatal("resolved pub does not match leaf pub")
	}
}

func TestTrustGraphRejectsUnanchored(t *testing.T) {
	_, orphanPriv, _ := ed25519.GenerateKey(rand.Reader)
	orphanPub := orphanPriv.Public().(ed25519.PublicKey)
	midPub, midPriv, _ := ed25519.GenerateKey(rand.Reader)
	leafPub, _, _ := ed25519.GenerateKey(rand.Reader)

	g := newTrustGraph()
	// no anchor — orphan is NOT in the anchor set
	g.addCert(e2e.SignCert(orphanPriv, "mid", "orphan", midPub))
	g.addCert(e2e.SignCert(midPriv, "leaf", "mid", leafPub))
	_ = orphanPub

	_, ok := g.resolve("leaf")
	if ok {
		t.Fatal("chain never reaching an anchor must not resolve")
	}
}

func TestTrustGraphRejectsBadSig(t *testing.T) {
	_, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	childPub, _, _ := ed25519.GenerateKey(rand.Reader)

	g := newTrustGraph()
	buildAnchorSet(t, g, adminPriv, "root", rootPub)

	// Sign a valid cert, then corrupt the signature.
	cert := e2e.SignCert(rootPriv, "child", "root", childPub)
	cert.Sig[0] ^= 0xFF
	g.addCert(cert)

	_, ok := g.resolve("child")
	if ok {
		t.Fatal("cert with bad signature must not resolve")
	}
}

func TestTrustGraphRejectsTamperedChildPub(t *testing.T) {
	_, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	rootPub, rootPriv, _ := ed25519.GenerateKey(rand.Reader)
	childPub, _, _ := ed25519.GenerateKey(rand.Reader)

	g := newTrustGraph()
	buildAnchorSet(t, g, adminPriv, "root", rootPub)

	cert := e2e.SignCert(rootPriv, "child", "root", childPub)
	// Tamper with the ChildSignPub after signing — sig won't verify.
	cert.ChildSignPub[0] ^= 0xFF
	g.addCert(cert)

	_, ok := g.resolve("child")
	if ok {
		t.Fatal("cert with tampered ChildSignPub must not resolve")
	}
}

func TestTrustGraphCycleSafe(t *testing.T) {
	// A.parent=B, B.parent=A — neither is an anchor.
	// resolve(A) must terminate and return false.
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)

	g := newTrustGraph()
	g.addCert(e2e.SignCert(privB, "A", "B", pubA))
	g.addCert(e2e.SignCert(privA, "B", "A", pubB))

	done := make(chan bool, 1)
	go func() {
		_, ok := g.resolve("A")
		done <- ok
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("cycle with no anchor must return false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolve hung on a cycle — not cycle-safe")
	}
}
