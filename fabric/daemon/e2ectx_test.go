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
	ctx, ok, err := d.e2eContextFor("leaf")
	if err != nil || ok || ctx != nil {
		t.Fatalf("non-e2e workspace must yield (nil,false,nil); got ctx=%v ok=%v err=%v", ctx, ok, err)
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

// stubNextCounterSeed replaces the package-level seed hook with one that
// always returns v, and returns a func that restores the original. Tests use
// this to get deterministic seed values instead of the real wall-clock
// nanosecond seed.
func stubNextCounterSeed(v uint64) (restore func()) {
	orig := nextCounterSeed
	nextCounterSeed = func() uint64 { return v }
	return func() { nextCounterSeed = orig }
}

func TestNextCounterMonotonicFromSeed(t *testing.T) {
	defer stubNextCounterSeed(1000)()

	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "root", Parent: ""}, {ID: "leaf", Parent: "root"}}}
	d := &Daemon{state: st, trust: newTrustGraph(), replay: newReplayWindow()}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := make([]byte, 32)
	rand.Read(key)
	ctx := &e2eCtx{channelID: "root", deviceID: "leaf", epoch: 1, devPriv: priv}

	// First call for a brand-new key must return the seed, not 1.
	c1, err := ctx.nextCounter(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c1 != 1000 {
		t.Fatalf("first counter must equal the seed (1000), got %d", c1)
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

	// Different key is independent and gets its own seed.
	ctx2 := &e2eCtx{channelID: "root", deviceID: "other-leaf", epoch: 1, devPriv: priv}
	c4, err := ctx2.nextCounter(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c4 != 1000 {
		t.Fatalf("different key must start at its own seed (1000), got %d", c4)
	}
}

// TestNextCounterSurvivesRestart is the regression test for the fix: after a
// simulated daemon restart (a brand-new Daemon with a nil in-memory counters
// map — exactly the state a process restart leaves behind), the sender's
// first counter for a (device,channel,epoch) triple that a peer has already
// seen messages for must still be accepted by that peer's replay window.
// Before the fix, nextCounter always restarted at 1, which a replay window
// that had already seen a higher counter would reject forever — a durable,
// silent, one-directional message drop until the counter climbed back past
// the old high-water mark or the workspace key rotated.
func TestNextCounterSurvivesRestart(t *testing.T) {
	// Simulate the pre-restart world: the receiver's replay window has
	// already accepted a run of messages up to counter 500 on this triple.
	rk := replayKey{device: "leaf", channel: "root", epoch: 1}
	w := newReplayWindow()
	if !w.accept(rk, 500) {
		t.Fatal("setup: replay window must accept the pre-restart high-water mark")
	}

	// Simulate the sender daemon restarting: a fresh Daemon with a nil
	// counters map, so nextCounter must treat this key as brand new.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	st := &agentstate.State{Agents: []agentstate.Agent{{ID: "root"}, {ID: "leaf", Parent: "root"}}}
	d := &Daemon{state: st, trust: newTrustGraph(), replay: newReplayWindow()}
	ctx := &e2eCtx{channelID: "root", deviceID: "leaf", epoch: 1, devPriv: priv}

	counter, err := ctx.nextCounter(d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counter <= 500 {
		t.Fatalf("post-restart counter %d must exceed the pre-restart high-water mark 500 (this is exactly the bug being fixed)", counter)
	}

	// The peer's (unchanged, still-running) replay window must accept it.
	if !w.accept(rk, counter) {
		t.Fatalf("post-restart counter %d was rejected by the peer's replay window — messages would be silently dropped", counter)
	}
}
