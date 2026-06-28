package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"
)

func TestRunWaitingRoomRecordsPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, fake, ws := newAdminDaemon(t)

	go runWaitingRoom(ctx, d, ws)
	time.Sleep(20 * time.Millisecond)

	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	encPub, _, _ := box.GenerateKey(rand.Reader)
	jb, _ := JoinRequest{ReqID: "r1", Name: "alice-laptop", SignPub: signPub, EncPub: encPub[:]}.Marshal()
	fake.Publish(ctx, ws.WaitingRoom, string(jb))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.pendingLen(ws) == 1 && d.pendingReqID(ws, 0) == "r1" {
			// A grant on the same channel must NOT be recorded as pending.
			gb, _ := AdmitGrant{ReqID: "r1", AnchorID: "r1"}.Marshal()
			fake.Publish(ctx, ws.WaitingRoom, string(gb))
			time.Sleep(50 * time.Millisecond)
			if d.pendingLen(ws) != 1 {
				t.Fatalf("grant polluted pending: len=%d", d.pendingLen(ws))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("join request not recorded: len=%d", d.pendingLen(ws))
}
