package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

func TestE2EContextForNonE2EReturnsFalse(t *testing.T) {
	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "leaf", Parent: "root"}, {ID: "root"}}}
	d := &Daemon{state: st}
	_, ok, err := d.e2eContextFor("leaf")
	if err != nil || ok {
		t.Fatalf("non-e2e workspace must yield (nil,false,nil); got ok=%v err=%v", ok, err)
	}
}

func TestE2EContextForE2EBuildsCtx(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := make([]byte, 32)
	rand.Read(key)
	st := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "root", Parent: ""},
			{ID: "leaf", Parent: "root", SignSeed: priv.Seed()},
		},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key}},
	}
	d := &Daemon{state: st}
	ctx, ok, err := d.e2eContextFor("leaf")
	if err != nil || !ok {
		t.Fatalf("expected e2e ctx; ok=%v err=%v", ok, err)
	}
	if ctx.channelID != "root" || ctx.deviceID != "leaf" || ctx.epoch != 1 {
		t.Fatalf("bad ctx: %+v", ctx)
	}
	if len(ctx.devPriv) != ed25519.PrivateKeySize {
		t.Fatal("device priv not derived from seed")
	}
}

func TestKeyArrayRejectsWrongLen(t *testing.T) {
	if _, err := keyArray([]byte("short")); err == nil {
		t.Fatal("expected error on non-32-byte key")
	}
}

func TestNextCounterMonotonicFromOne(t *testing.T) {
	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "root", Parent: ""}, {ID: "leaf", Parent: "root"}}}
	d := &Daemon{state: st, devices: newDeviceSet(), replay: newReplayWindow()}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := make([]byte, 32)
	rand.Read(key)
	ctx := &e2eCtx{channelID: "root", deviceID: "leaf", epoch: 1, devPriv: priv}

	// First call must return 1.
	c1, err := ctx.nextCounter(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c1 != 1 {
		t.Fatalf("first counter must be 1, got %d", c1)
	}

	// Subsequent calls strictly increase.
	c2, err := ctx.nextCounter(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c2 <= c1 {
		t.Fatalf("counter must strictly increase: c1=%d c2=%d", c1, c2)
	}

	c3, err := ctx.nextCounter(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c3 <= c2 {
		t.Fatalf("counter must strictly increase: c2=%d c3=%d", c2, c3)
	}

	// Different key is independent (starts at 1).
	ctx2 := &e2eCtx{channelID: "root", deviceID: "other-leaf", epoch: 1, devPriv: priv}
	c4, err := ctx2.nextCounter(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c4 != 1 {
		t.Fatalf("different key must start at 1, got %d", c4)
	}
}
