package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/e2e"
	"github.com/ericpollmann/botbus-proto/envelope"
)

func sealedEnvelope(t *testing.T, key [32]byte, epoch uint32, channelID, deviceID string, priv ed25519.PrivateKey, counter uint64, subject, body string) envelope.Envelope {
	t.Helper()
	env, err := e2e.SealMessage(key, epoch, channelID, deviceID, priv, counter, encodeContent(subject, body))
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Envelope{V: 1, ID: "m1", From: deviceID, Kind: envelope.KindChat,
		Enc: base64.StdEncoding.EncodeToString(env.Marshal())}
}

func TestOpenerDecryptsValidMessage(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var key [32]byte
	rand.Read(key[:])
	st := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}, {ID: "bob", Parent: "root", SignSeed: priv.Seed()}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 7, Key: key[:]}},
	}
	d := &Daemon{state: st, devices: newDeviceSet(), replay: newReplayWindow()}
	d.devices.set("alice", pub) // sender's device pubkey known to receiver

	env := sealedEnvelope(t, key, 7, "root", "alice", priv, 1, "hi subj", "hi body")
	open := d.openerFor("bob")
	got, ok := open(env)
	if !ok {
		t.Fatal("valid message dropped")
	}
	if got.Subject != "hi subj" || got.Body != "hi body" {
		t.Fatalf("decrypt mismatch: %+v", got)
	}
}

func TestOpenerDropsReplay(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var key [32]byte
	rand.Read(key[:])
	st := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}, {ID: "bob", Parent: "root", SignSeed: priv.Seed()}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 7, Key: key[:]}},
	}
	d := &Daemon{state: st, devices: newDeviceSet(), replay: newReplayWindow()}
	d.devices.set("alice", pub)
	env := sealedEnvelope(t, key, 7, "root", "alice", priv, 1, "s", "b")
	open := d.openerFor("bob")
	if _, ok := open(env); !ok {
		t.Fatal("first delivery should pass")
	}
	if _, ok := open(env); ok {
		t.Fatal("replayed counter must be dropped")
	}
}

func TestOpenerDropsUnknownDevice(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var key [32]byte
	rand.Read(key[:])
	st := &agentstate.State{
		Agents:     []agentstate.Agent{{ID: "root"}, {ID: "bob", Parent: "root", SignSeed: priv.Seed()}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 7, Key: key[:]}},
	}
	d := &Daemon{state: st, devices: newDeviceSet(), replay: newReplayWindow()} // empty device set
	env := sealedEnvelope(t, key, 7, "root", "alice", priv, 1, "s", "b")
	if _, ok := d.openerFor("bob")(env); ok {
		t.Fatal("unknown device must be dropped")
	}
}
