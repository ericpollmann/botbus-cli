package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"golang.org/x/crypto/nacl/box"
)

func TestRunRosterAdoptsRekey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Admin admits Bob; build Bob's daemon as the loop's host.
	dAdmin, fake, ws := newAdminDaemon(t)
	bobSignPub, bobSignSeed, _ := ed25519.GenerateKey(rand.Reader)
	bobEncPub, bobEncPriv, _ := box.GenerateKey(rand.Reader)
	_, _ = dAdmin.AdmitJoinRequest(ctx, ws, JoinRequest{ReqID: "bob", SignPub: bobSignPub, EncPub: bobEncPub[:]})

	bobState := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "bob", SignSeed: bobSignSeed.Seed(), EncPriv: bobEncPriv[:]}},
		Workspaces: []agentstate.Workspace{{RootID: "bob", E2E: true, Epoch: ws.Epoch, Key: append([]byte(nil), ws.Key...), AdminPub: append([]byte(nil), ws.AdminPub...), Roster: ws.Roster}},
	}
	dBob := &Daemon{state: bobState, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	bobWs := &bobState.Workspaces[0]

	go runRoster(ctx, dBob, bobWs) // subscribe BEFORE publishing (Fake has no replay)
	time.Sleep(20 * time.Millisecond)

	if _, err := dAdmin.RotateKey(ctx, ws); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k, ok := dBob.currentKey(bobWs); ok && k == mustKey(t, ws.Key) {
			return // adopted
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runRoster did not adopt the rotated key")
}
