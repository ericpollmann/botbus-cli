package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/e2e"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestTwoDaemonE2EConvergenceRelayBlind(t *testing.T) {
	// Shared workspace key + admin key; Alice (daemon A) sends, Bob (daemon B) reads.
	var key [32]byte
	rand.Read(key[:])
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	_, bobPriv, _ := ed25519.GenerateKey(rand.Reader)

	fake := hubclient.NewFake()

	mkState := func(self string, seed []byte) *agentstate.State {
		return &agentstate.State{
			Daemon: agentstate.Daemon{OutboundChannel: "out"},
			Agents: []agentstate.Agent{
				{ID: "root"},
				{ID: self, Parent: "root", SignSeed: seed, InboxChannel: "inbox-" + self},
			},
			Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key[:], AdminPub: adminPub}},
		}
	}
	dA := &Daemon{state: mkState("alice", alicePriv.Seed()), hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	dB := &Daemon{state: mkState("bob", bobPriv.Seed()), hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}

	// Admin-signed anchor set naming alice, delivered to B (the real path
	// is the roster channel; here inject the signed blob B verifies).
	blob := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"alice": alicePub}})
	sig := ed25519.Sign(adminPriv, blob)
	if err := dB.trust.applyAnchorSet(blob, sig, adminPub); err != nil {
		t.Fatal(err)
	}

	// Alice sends.
	if err := dA.Send(context.Background(), "alice", SendArgs{Subject: "topsecret", Body: "the eagle lands at noon"}); err != nil {
		t.Fatal(err)
	}

	// Relay-blind assertion: the frame on the hub carries no plaintext.
	raw := fake.Published("out")[0]
	if strings.Contains(raw, "eagle") || strings.Contains(raw, "topsecret") {
		t.Fatalf("relay saw plaintext: %s", raw)
	}

	// Deliver the frame to Bob's inbox and open it.
	body := raw[strings.Index(raw, ": ")+2:]
	e, _ := envelope.Decode([]byte(body))
	got, ok := dB.openerFor("bob")(e)
	if !ok {
		t.Fatal("bob could not open alice's message")
	}
	if got.Subject != "topsecret" || got.Body != "the eagle lands at noon" {
		t.Fatalf("convergence mismatch: %+v", got)
	}

	// Wrong-key daemon (different workspace key) cannot read.
	var otherKey [32]byte
	rand.Read(otherKey[:])
	dC := &Daemon{
		state: &agentstate.State{Agents: []agentstate.Agent{{ID: "root"}, {ID: "carol", Parent: "root", SignSeed: bobPriv.Seed()}}, Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: otherKey[:], AdminPub: adminPub}}},
		trust: dB.trust, replay: newReplayWindow(),
	}
	if _, ok := dC.openerFor("carol")(e); ok {
		t.Fatal("a daemon with the wrong workspace key must not decrypt")
	}
}

// TestTwoDaemonE2ETamperedCiphertextDropped seals a valid message as Alice,
// then corrupts the ciphertext by flipping a byte in the AEAD ciphertext/tag
// region. Bob's opener must return ok==false because the Poly1305 tag rejects
// the tamper.
func TestTwoDaemonE2ETamperedCiphertextDropped(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	_, bobPriv, _ := ed25519.GenerateKey(rand.Reader)

	fake := hubclient.NewFake()

	mkState := func(self string, seed []byte) *agentstate.State {
		return &agentstate.State{
			Daemon: agentstate.Daemon{OutboundChannel: "out"},
			Agents: []agentstate.Agent{
				{ID: "root"},
				{ID: self, Parent: "root", SignSeed: seed, InboxChannel: "inbox-" + self},
			},
			Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key[:], AdminPub: adminPub}},
		}
	}
	dA := &Daemon{state: mkState("alice", alicePriv.Seed()), hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	dB := &Daemon{state: mkState("bob", bobPriv.Seed()), hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}

	blob := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"alice": alicePub}})
	sig := ed25519.Sign(adminPriv, blob)
	if err := dB.trust.applyAnchorSet(blob, sig, adminPub); err != nil {
		t.Fatal(err)
	}

	// Alice sends a valid message.
	if err := dA.Send(context.Background(), "alice", SendArgs{Subject: "secret", Body: "attack at dawn"}); err != nil {
		t.Fatal(err)
	}

	raw := fake.Published("out")[0]
	body := raw[strings.Index(raw, ": ")+2:]
	e, err := envelope.Decode([]byte(body))
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if e.Enc == "" {
		t.Fatal("expected encrypted frame")
	}

	// Corrupt the ciphertext: decode Enc, flip a byte near the tail (AEAD CT/tag region).
	ctBytes, err := base64.StdEncoding.DecodeString(e.Enc)
	if err != nil {
		t.Fatalf("base64 decode Enc: %v", err)
	}
	if len(ctBytes) < 1 {
		t.Fatal("ciphertext too short to tamper")
	}
	// Flip the last byte — that's solidly in the Poly1305 tag (16 bytes at end).
	ctBytes[len(ctBytes)-1] ^= 0xFF
	e.Enc = base64.StdEncoding.EncodeToString(ctBytes)

	// Bob's opener must drop it.
	_, ok := dB.openerFor("bob")(e)
	if ok {
		t.Fatal("tampered ciphertext must not decrypt successfully (AEAD tag should reject it)")
	}
}

// TestE2EAgentDropsCleartextFrame asserts the fail-closed decision: an
// e2e-configured agent's opener DROPS all unencrypted inbound frames (Enc=="").
// A compromised relay cannot inject unauthenticated cleartext to an e2e agent.
// The connect welcome is delivered locally and never traverses the relay.
func TestE2EAgentDropsCleartextFrame(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	st := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "root"},
			{ID: "bob", Parent: "root", SignSeed: priv.Seed()},
		},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key[:]}},
	}
	dB := &Daemon{state: st, trust: newTrustGraph(), replay: newReplayWindow()}

	// Construct a cleartext envelope (Enc == "").
	cleartext := envelope.Envelope{
		V:    1,
		ID:   "cleartext-1",
		From: "unknown-sender",
		Kind: envelope.KindChat,
		Body: "hello in the clear",
	}

	_, ok := dB.openerFor("bob")(cleartext)
	// Fail-closed: cleartext frames must be dropped for e2e agents.
	if ok {
		t.Fatal("e2e agent must drop cleartext frames (fail-closed)")
	}
}

// TestOpenerAcceptsRemoteAgentViaCertChain verifies that a remote agent whose
// signing key is certified by an admitted anchor is accepted by the receiver's
// opener. The remote agent ("carol") is not in the local agent state; it was
// onboarded on a different host. The receiving daemon ("bob") knows alice as an
// anchor and has a cert alice → carol; carol seals a message; bob opens it.
func TestOpenerAcceptsRemoteAgentViaCertChain(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	adminPub, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	alicePub, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	carolPub, carolPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, bobPriv, _ := ed25519.GenerateKey(rand.Reader)

	// Receiver: bob's daemon. Workspace root is "root"; bob is a child.
	st := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "root"},
			{ID: "bob", Parent: "root", SignSeed: bobPriv.Seed(), InboxChannel: "inbox-bob"},
		},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key[:], AdminPub: adminPub}},
	}
	dB := &Daemon{state: st, trust: newTrustGraph(), replay: newReplayWindow()}

	// Admit alice as an anchor via admin-signed blob.
	blob := marshalDeviceSet(signedDeviceSet{Epoch: 1, Devices: map[string][]byte{"alice": alicePub}})
	sig := ed25519.Sign(adminPriv, blob)
	if err := dB.trust.applyAnchorSet(blob, sig, adminPub); err != nil {
		t.Fatalf("applyAnchorSet: %v", err)
	}

	// Alice certifies carol (carol is on a different host — not in dB's agentstate).
	dB.trust.addCert(e2e.SignCert(alicePriv, "carol", "alice", carolPub))

	// Carol seals a message using the shared workspace key. The channelID is the
	// workspace RootID (same convention as same-host messages).
	channelID := "root"
	env, err := e2e.SealMessage(key, 1, channelID, "carol", carolPriv, 1, encodeContent("remote-subj", "remote-body"))
	if err != nil {
		t.Fatalf("SealMessage: %v", err)
	}
	wrapped := envelope.Envelope{V: 1, ID: "m-carol", From: "carol", Kind: envelope.KindChat,
		Enc: base64.StdEncoding.EncodeToString(env.Marshal())}

	got, ok := dB.openerFor("bob")(wrapped)
	if !ok {
		t.Fatal("opener must accept remote agent carol with cert from admitted anchor alice")
	}
	if got.Subject != "remote-subj" || got.Body != "remote-body" {
		t.Fatalf("decrypted content mismatch: %+v", got)
	}
}

// TestOpenerDropsUnanchoredRemoteAgent verifies that a remote agent whose cert
// chain does not trace to an admitted anchor is rejected. "dave" has a cert
// from "mallory", but mallory is not an anchor and has no cert of her own.
func TestOpenerDropsUnanchoredRemoteAgent(t *testing.T) {
	var key [32]byte
	rand.Read(key[:])
	_, adminPriv, _ := ed25519.GenerateKey(rand.Reader)
	adminPub := adminPriv.Public().(ed25519.PublicKey)
	malloryPub, malloryPriv, _ := ed25519.GenerateKey(rand.Reader)
	davePub, davePriv, _ := ed25519.GenerateKey(rand.Reader)
	_, bobPriv, _ := ed25519.GenerateKey(rand.Reader)

	st := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "root"},
			{ID: "bob", Parent: "root", SignSeed: bobPriv.Seed(), InboxChannel: "inbox-bob"},
		},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key[:], AdminPub: adminPub}},
	}
	dB := &Daemon{state: st, trust: newTrustGraph(), replay: newReplayWindow()}

	// No anchors admitted for mallory — she is a stranger.
	// Dave has a cert signed by mallory, but mallory is not trusted.
	_ = malloryPub // used only via cert
	dB.trust.addCert(e2e.SignCert(malloryPriv, "dave", "mallory", davePub))

	channelID := "root"
	env, err := e2e.SealMessage(key, 1, channelID, "dave", davePriv, 1, encodeContent("s", "b"))
	if err != nil {
		t.Fatalf("SealMessage: %v", err)
	}
	wrapped := envelope.Envelope{V: 1, ID: "m-dave", From: "dave", Kind: envelope.KindChat,
		Enc: base64.StdEncoding.EncodeToString(env.Marshal())}

	if _, ok := dB.openerFor("bob")(wrapped); ok {
		t.Fatal("opener must drop dave whose cert chain does not reach an admitted anchor")
	}
}
