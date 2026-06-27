package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/e2e"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// TestRosterFrameRoundTrip verifies that sealRosterFrame→openRosterFrame
// round-trips a cert frame and an anchors frame correctly, and that a wrong
// key fails to open.
func TestRosterFrameRoundTrip(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	cert := e2e.SignCert(priv, "child", "parent", pub)

	// Cert frame round-trip.
	t.Run("cert", func(t *testing.T) {
		b64, err := sealRosterFrame(key, 1, rosterFrame{Kind: "cert", Cert: &cert})
		if err != nil {
			t.Fatalf("sealRosterFrame: %v", err)
		}
		got, err := openRosterFrame(key, b64)
		if err != nil {
			t.Fatalf("openRosterFrame: %v", err)
		}
		if got.Kind != "cert" {
			t.Fatalf("kind: got %q, want %q", got.Kind, "cert")
		}
		if got.Cert == nil {
			t.Fatal("cert is nil after round-trip")
		}
		if got.Cert.ChildID != cert.ChildID || got.Cert.ParentID != cert.ParentID {
			t.Fatalf("cert mismatch: %+v", got.Cert)
		}
	})

	// Anchors frame round-trip.
	t.Run("anchors", func(t *testing.T) {
		blob := []byte("some-anchor-blob")
		sig := []byte("some-anchor-sig")
		b64, err := sealRosterFrame(key, 1, rosterFrame{Kind: "anchors", AnchorBlob: blob, AnchorSig: sig})
		if err != nil {
			t.Fatalf("sealRosterFrame: %v", err)
		}
		got, err := openRosterFrame(key, b64)
		if err != nil {
			t.Fatalf("openRosterFrame: %v", err)
		}
		if got.Kind != "anchors" {
			t.Fatalf("kind: got %q, want %q", got.Kind, "anchors")
		}
		if string(got.AnchorBlob) != "some-anchor-blob" || string(got.AnchorSig) != "some-anchor-sig" {
			t.Fatalf("anchors mismatch: blob=%q sig=%q", got.AnchorBlob, got.AnchorSig)
		}
	})

	// Wrong key must fail.
	t.Run("wrong_key", func(t *testing.T) {
		b64, err := sealRosterFrame(key, 1, rosterFrame{Kind: "cert", Cert: &cert})
		if err != nil {
			t.Fatalf("sealRosterFrame: %v", err)
		}
		var wrongKey [32]byte
		rand.Read(wrongKey[:])
		_, err = openRosterFrame(wrongKey, b64)
		if err == nil {
			t.Fatal("expected error opening with wrong key, got nil")
		}
	})
}

// TestCrossHostPublishIngestResolve is the key cross-host test: daemon A
// publishCerts a cert for its child agent to the roster channel; daemon B
// (same workspace key, anchor already admitted) ingestRosterFrames it and
// can then resolve the child's id via trust.resolve.
func TestCrossHostPublishIngestResolve(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	parentPub, parentPriv, _ := ed25519.GenerateKey(rand.Reader)
	childPub, _, _ := ed25519.GenerateKey(rand.Reader)

	fake := hubclient.NewFake()
	rosterChannel := "roster-channel-123"

	ws := agentstate.Workspace{
		RootID:   "root",
		E2E:      true,
		Epoch:    1,
		Key:      key[:],
		AdminPub: []byte(adminPub),
		Roster:   rosterChannel,
	}

	// Daemon A: has the child cert locally, publishes it.
	stateA := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "root", SignSeed: parentPriv.Seed()},
			{ID: "child-agent", Parent: "root"},
		},
		Workspaces: []agentstate.Workspace{ws},
	}
	dA := &Daemon{state: stateA, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}

	// Daemon B: admits "root" as an anchor, does NOT yet have child's cert.
	stateB := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "root"},
		},
		Workspaces: []agentstate.Workspace{ws},
	}
	dB := &Daemon{state: stateB, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}

	// Admit "root" (parentPub) as an anchor on dB.
	blob := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"root": parentPub}})
	sig := ed25519.Sign(adminPriv, blob)
	if err := dB.trust.applyAnchorSet(blob, sig, adminPub); err != nil {
		t.Fatalf("applyAnchorSet: %v", err)
	}

	// Before publish, dB cannot resolve child-agent.
	if _, ok := dB.trust.resolve("child-agent"); ok {
		t.Fatal("child-agent should not be resolvable before publishCert")
	}

	// Build the cert: parentPriv certifies child-agent's pubkey.
	cert := e2e.SignCert(parentPriv, "child-agent", "root", childPub)

	// Daemon A publishes the cert.
	if err := dA.publishCert(context.Background(), &ws, cert); err != nil {
		t.Fatalf("publishCert: %v", err)
	}

	// Verify a frame was published to the roster channel.
	frames := fake.Published(rosterChannel)
	if len(frames) != 1 {
		t.Fatalf("expected 1 published frame on roster channel, got %d", len(frames))
	}

	// Daemon B ingests the frame.
	dB.ingestRosterFrame(&ws, frames[0])

	// Now dB should be able to resolve child-agent.
	resolvedPub, ok := dB.trust.resolve("child-agent")
	if !ok {
		t.Fatal("child-agent should be resolvable after ingestRosterFrame")
	}
	wantPub := childPub
	if string(resolvedPub) != string(wantPub) {
		t.Fatal("resolved pubkey does not match expected childPub")
	}
}

// TestCrossHostWrongKeyFrameDropped verifies that a roster frame sealed under
// a different key does not add anything to the trust graph.
func TestCrossHostWrongKeyFrameDropped(t *testing.T) {
	var correctKey [32]byte
	var wrongKey [32]byte
	rand.Read(correctKey[:])
	rand.Read(wrongKey[:])

	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	parentPub, parentPriv, _ := ed25519.GenerateKey(rand.Reader)
	childPub, _, _ := ed25519.GenerateKey(rand.Reader)

	rosterChannel := "roster-channel-456"
	fake := hubclient.NewFake()

	// Workspace with CORRECT key.
	wsCorrect := agentstate.Workspace{
		RootID:   "root",
		E2E:      true,
		Epoch:    1,
		Key:      correctKey[:],
		AdminPub: []byte(adminPub),
		Roster:   rosterChannel,
	}
	// Workspace with WRONG key — used to seal the frame.
	wsWrong := agentstate.Workspace{
		RootID:   "root",
		E2E:      true,
		Epoch:    1,
		Key:      wrongKey[:],
		AdminPub: []byte(adminPub),
		Roster:   rosterChannel,
	}

	// Daemon A seals under the WRONG key.
	stateA := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}},
		Workspaces: []agentstate.Workspace{wsWrong},
	}
	dA := &Daemon{state: stateA, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}

	cert := e2e.SignCert(parentPriv, "child-agent", "root", childPub)
	if err := dA.publishCert(context.Background(), &wsWrong, cert); err != nil {
		t.Fatalf("publishCert: %v", err)
	}

	// Daemon B has the CORRECT key and admits root as anchor.
	stateB := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}},
		Workspaces: []agentstate.Workspace{wsCorrect},
	}
	dB := &Daemon{state: stateB, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}

	blob := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"root": parentPub}})
	sig := ed25519.Sign(adminPriv, blob)
	if err := dB.trust.applyAnchorSet(blob, sig, adminPub); err != nil {
		t.Fatalf("applyAnchorSet: %v", err)
	}

	// Ingest the frame sealed under the wrong key.
	frames := fake.Published(rosterChannel)
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	dB.ingestRosterFrame(&wsCorrect, frames[0])

	// child-agent must NOT be resolvable.
	if _, ok := dB.trust.resolve("child-agent"); ok {
		t.Fatal("child-agent must not be resolvable when frame was sealed under wrong key")
	}
}
