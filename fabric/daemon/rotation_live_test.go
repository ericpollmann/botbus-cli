package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"golang.org/x/crypto/nacl/box"
)

func TestOpenerAdoptsRotatedKeyLive(t *testing.T) {
	ctx := context.Background()
	// --- Admin (Alice) workspace with one admitted anchor (Bob). ---
	d, fake, ws := newAdminDaemon(t)
	bobSignPub, bobSignSeed, _ := ed25519.GenerateKey(rand.Reader)
	bobEncPub, bobEncPriv, _ := box.GenerateKey(rand.Reader)
	if _, err := d.AdmitJoinRequest(ctx, ws, JoinRequest{ReqID: "bob", SignPub: bobSignPub, EncPub: bobEncPub[:]}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	// --- Bob's daemon: knows the workspace (epoch 1 key) + holds bob's enc/sign keys. ---
	adminPub := append([]byte(nil), ws.AdminPub...)
	bobState := &agentstate.State{
		Agents: []agentstate.Agent{{ID: "bob", SignSeed: bobSignSeed.Seed(), EncPriv: bobEncPriv[:], InboxChannel: "bob-inbox"}},
		Workspaces: []agentstate.Workspace{{
			RootID: "bob", E2E: true, Epoch: ws.Epoch, Key: append([]byte(nil), ws.Key...),
			AdminPub: adminPub, Roster: ws.Roster, WaitingRoom: ws.WaitingRoom,
		}},
	}
	dBob := &Daemon{state: bobState, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	bobWs := &bobState.Workspaces[0]
	// Bob trusts admin's anchor set so it can verify Alice's signatures.
	dBob.hydrateWorkspaceTrust(bobWs)
	dBob.trust.anchors = d.trust.anchors // share the admin-signed anchor set for the test

	// --- Admin rotates; capture the rekey grant aimed at bob and feed Bob's ingest. ---
	before := len(fake.Published("roster"))
	if _, err := d.RotateKey(ctx, ws); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	for _, f := range fake.Published("roster")[before:] {
		dBob.ingestRosterFrame(bobWs, f) // grants adopt; sealed frames need the new key (adopted first)
	}
	gotKey, ok := dBob.currentKey(bobWs)
	if !ok || gotKey != [32]byte(mustKey(t, ws.Key)) {
		t.Fatal("Bob did not adopt the rotated key via roster ingest")
	}
	if bobWs.Epoch != ws.Epoch {
		t.Fatalf("Bob epoch=%d want %d", bobWs.Epoch, ws.Epoch)
	}
}

func mustKey(t *testing.T, b []byte) [32]byte {
	t.Helper()
	var k [32]byte
	if len(b) != 32 {
		t.Fatalf("bad key len %d", len(b))
	}
	copy(k[:], b)
	return k
}
