package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestSendE2EHidesContentFromRelay(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := make([]byte, 32)
	rand.Read(key)
	fake := hubclient.NewFake()
	st := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out"},
		Agents: []agentstate.Agent{
			{ID: "root", Parent: ""},
			{ID: "alice", Parent: "root", SignSeed: priv.Seed()},
		},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: key}},
	}
	d := &Daemon{state: st, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}

	if err := d.Send(context.Background(), "alice", SendArgs{Subject: "secret subj", Body: "secret body"}); err != nil {
		t.Fatal(err)
	}
	frames := fake.Published("out")
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	// frame is "alice: <json>"
	raw := frames[0][strings.Index(frames[0], ": ")+2:]
	e, err := envelope.Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "" || e.Body != "" {
		t.Fatalf("plaintext leaked: subj=%q body=%q", e.Subject, e.Body)
	}
	if e.Enc == "" {
		t.Fatal("expected ciphertext in Enc")
	}
	if strings.Contains(raw, "secret") {
		t.Fatalf("plaintext substring leaked into frame: %s", raw)
	}
}

func TestSendNonE2EUnchanged(t *testing.T) {
	fake := hubclient.NewFake()
	st := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out"},
		Agents: []agentstate.Agent{{ID: "bob", Parent: ""}},
	}
	d := &Daemon{state: st, hub: fake, trust: newTrustGraph(), replay: newReplayWindow()}
	if err := d.Send(context.Background(), "bob", SendArgs{Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	raw := fake.Published("out")[0]
	if !strings.Contains(raw, `"body":"b"`) {
		t.Fatalf("non-e2e body must be cleartext: %s", raw)
	}
}
