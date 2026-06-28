package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

// buildJoinerState constructs an agentstate.State for a joiner host.
// The joiner has a single agent whose parent chain root is the admin workspace's
// RootID, so WorkspaceFor(joinerAgentID) correctly resolves the shared workspace.
// The workspace is pre-populated from the AdmitGrant.
func buildJoinerState(t *testing.T, joinerAgentID string, grant AdmitGrant, signPriv ed25519.PrivateKey, encPriv *[32]byte) (*agentstate.State, *agentstate.Workspace) {
	t.Helper()
	populated, _, ok := ProcessAdmitGrant(grant, encPriv[:], nil)
	if !ok {
		t.Fatalf("ProcessAdmitGrant failed for %s", joinerAgentID)
	}
	// The joiner's agent hierarchy: a root placeholder + the real agent as its child.
	// WorkspaceFor walks Parent links until Parent==""; the workspace matches on RootID.
	joinerState := &agentstate.State{
		Daemon: agentstate.Daemon{OutboundChannel: "out-" + joinerAgentID},
		Agents: []agentstate.Agent{
			// Root placeholder so WorkspaceFor(joinerAgentID) resolves to grant.RootID.
			{ID: grant.RootID},
			{
				ID:           joinerAgentID,
				Parent:       grant.RootID,
				SignSeed:     signPriv.Seed(),
				EncPriv:      encPriv[:],
				InboxChannel: "inbox-" + joinerAgentID,
			},
		},
		Workspaces: []agentstate.Workspace{*populated},
	}
	return joinerState, &joinerState.Workspaces[0]
}

// newJoinerDaemon creates a joiner Daemon that has been admitted into adminWs,
// with its trust graph seeded from the admin's published roster frames.
func newJoinerDaemon(
	t *testing.T,
	joinerAgentID string,
	dAdmin *Daemon,
	adminWs *agentstate.Workspace,
	fake *hubclient.Fake,
) (
	d *Daemon,
	ws *agentstate.Workspace,
	signPub ed25519.PublicKey,
	encPriv *[32]byte,
) {
	t.Helper()
	ctx := context.Background()

	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey sign: %v", err)
	}
	var encPub *[32]byte
	encPub, encPriv, err = box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey enc: %v", err)
	}

	grant, err := dAdmin.AdmitJoinRequest(ctx, adminWs, JoinRequest{
		ReqID:   joinerAgentID,
		Name:    joinerAgentID,
		SignPub: signPub,
		EncPub:  encPub[:],
	})
	if err != nil {
		t.Fatalf("AdmitJoinRequest(%s): %v", joinerAgentID, err)
	}

	joinerState, joinerWs := buildJoinerState(t, joinerAgentID, grant, signPriv, encPriv)
	d = &Daemon{
		state:  joinerState,
		hub:    fake,
		trust:  newTrustGraph(),
		replay: newReplayWindow(),
	}
	ws = joinerWs

	// Seed the joiner's trust graph from the admin's published roster frames.
	d.hydrateWorkspaceTrust(ws)
	d.trust.anchors = dAdmin.trust.anchors // share admin's signed anchor snapshot

	return
}

// relayBlindCheck asserts that neither keyBytes nor msgBody appear in any
// published messages on the given channels.
func relayBlindCheck(t *testing.T, fake *hubclient.Fake, channels []string, keyBytes []byte, msgBody string) {
	t.Helper()
	for _, ch := range channels {
		for _, msg := range fake.Published(ch) {
			if len(keyBytes) > 0 && bytes.Contains([]byte(msg), keyBytes) {
				t.Fatalf("relay-blind violation on channel %q: plaintext key bytes found", ch)
			}
			if msgBody != "" && strings.Contains(msg, msgBody) {
				t.Fatalf("relay-blind violation on channel %q: plaintext message body %q found", ch, msgBody)
			}
		}
	}
}

// adminReceiverFor adds a minimal receiver agent to the admin daemon and returns
// its openerFor closure. The agent is a child of the admin workspace root so
// WorkspaceFor resolves it correctly.
func adminReceiverFor(dAdmin *Daemon, adminWs *agentstate.Workspace, receiverID string) opener {
	dAdmin.state.Agents = append(dAdmin.state.Agents, agentstate.Agent{
		ID:       receiverID,
		Parent:   adminWs.RootID,
		SignSeed: make([]byte, ed25519.SeedSize), // placeholder — only needed for sealing
	})
	return dAdmin.openerFor(receiverID)
}

// extractEnvelope pulls the last published frame from the given outbound channel
// and decodes it into an envelope.Envelope.
func extractLastEnvelope(t *testing.T, fake *hubclient.Fake, channel string) envelope.Envelope {
	t.Helper()
	pubs := fake.Published(channel)
	if len(pubs) == 0 {
		t.Fatalf("no published frames on channel %q", channel)
	}
	raw := pubs[len(pubs)-1]
	// Router delivery format: "FROMID: <json-envelope>"
	sep := strings.Index(raw, ": ")
	if sep < 0 {
		t.Fatalf("unexpected frame format (no ': ') on channel %q: %s", channel, raw)
	}
	e, err := envelope.Decode([]byte(raw[sep+2:]))
	if err != nil {
		t.Fatalf("envelope.Decode on channel %q: %v", channel, err)
	}
	return e
}

// TestJoinAdmitConvergeTwoHosts: admin + joiner share one hubclient.Fake.
// Joiner posts JoinRequest; admin admits via AdmitJoinRequest; joiner processes
// the grant; joiner sends an e2e message; admin decrypts it.
// Asserts relay-blind: neither workspace key bytes nor message body appear in
// any published frame.
func TestJoinAdmitConvergeTwoHosts(t *testing.T) {
	ctx := context.Background()
	dAdmin, fake, adminWs := newAdminDaemon(t)

	dJoiner, _, _, _ := newJoinerDaemon(t, "joiner1", dAdmin, adminWs, fake)

	// Drain roster/wroom channels so relay-blind check only sees post-admission frames.
	fake.Published(adminWs.Roster)
	fake.Published(adminWs.WaitingRoom)

	const secret = "the eagle lands at noon"
	if err := dJoiner.Send(ctx, "joiner1", SendArgs{Subject: "topsecret", Body: secret}); err != nil {
		t.Fatalf("joiner Send: %v", err)
	}

	// Relay-blind: key bytes and message body must not appear in cleartext.
	allChannels := []string{"out-joiner1", adminWs.Roster, adminWs.WaitingRoom}
	relayBlindCheck(t, fake, allChannels, adminWs.Key, secret)
	relayBlindCheck(t, fake, allChannels, adminWs.Key, "topsecret")

	// Admin decrypts the joiner's message.
	e := extractLastEnvelope(t, fake, "out-joiner1")
	open := adminReceiverFor(dAdmin, adminWs, "admin-recv-1")
	got, ok := open(e)
	if !ok {
		t.Fatal("admin could not open joiner's message")
	}
	if got.Subject != "topsecret" || got.Body != secret {
		t.Fatalf("convergence mismatch: subject=%q body=%q", got.Subject, got.Body)
	}
}

// TestRotateConvergesAtNewEpoch: after join+admit, admin RotateKey; joiner's
// runRoster loop adopts the new key; post-rotation message from joiner decrypts
// on admin; joiner workspace Epoch advanced to match admin.
func TestRotateConvergesAtNewEpoch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dAdmin, fake, adminWs := newAdminDaemon(t)
	dJoiner, joinerWs, _, _ := newJoinerDaemon(t, "joiner-r", dAdmin, adminWs, fake)

	epochBefore := joinerWs.Epoch

	// Start joiner's roster loop BEFORE rotation (Fake has no replay).
	go runRoster(ctx, dJoiner, joinerWs)
	time.Sleep(20 * time.Millisecond)

	// Admin rotates.
	newKey, err := dAdmin.RotateKey(ctx, adminWs)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// Wait for joiner's roster loop to adopt the new key (≤2 s).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k, ok := dJoiner.currentKey(joinerWs); ok && k == newKey {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gotKey, ok := dJoiner.currentKey(joinerWs)
	if !ok || gotKey != newKey {
		t.Fatalf("joiner did not adopt rotated key: epoch before=%d after=%d", epochBefore, joinerWs.Epoch)
	}
	if joinerWs.Epoch != adminWs.Epoch {
		t.Fatalf("joiner epoch %d != admin epoch %d", joinerWs.Epoch, adminWs.Epoch)
	}

	// Post-rotation: joiner sends a message; admin decrypts it.
	const postMsg = "post-rotation payload"
	if err := dJoiner.Send(ctx, "joiner-r", SendArgs{Subject: "post-rotate", Body: postMsg}); err != nil {
		t.Fatalf("post-rotate Send: %v", err)
	}

	e := extractLastEnvelope(t, fake, "out-joiner-r")
	open := adminReceiverFor(dAdmin, adminWs, "admin-recv-r")
	got, ok := open(e)
	if !ok {
		t.Fatal("admin could not open post-rotation message from joiner")
	}
	if got.Body != postMsg {
		t.Fatalf("body mismatch: got %q want %q", got.Body, postMsg)
	}

	// Relay-blind: new key must not appear in cleartext.
	allChannels := []string{"out-joiner-r", adminWs.Roster, adminWs.WaitingRoom}
	relayBlindCheck(t, fake, allChannels, newKey[:], postMsg)
}

// TestRemoveEvictsFromNewEpoch: admit two joiners (A, B); RemoveAnchor(B);
// A adopts the new key and can still converge; B does NOT receive a rekey grant
// at the new epoch and therefore cannot unwrap the new key or decrypt a message
// sent by A under the new epoch.
func TestRemoveEvictsFromNewEpoch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dAdmin, fake, adminWs := newAdminDaemon(t)
	dA, aWs, _, _ := newJoinerDaemon(t, "joiner-a", dAdmin, adminWs, fake)
	_, _, _, bEncPriv := newJoinerDaemon(t, "joiner-b", dAdmin, adminWs, fake)

	// Capture B's old epoch key (before removal).
	oldKey := mustKey(t, aWs.Key) // same key for both joiners at this point

	// Start A's roster loop before rotation.
	go runRoster(ctx, dA, aWs)
	time.Sleep(20 * time.Millisecond)

	// Remove B — this triggers RotateKey, publishing rekey grants only for A.
	newKey, err := dAdmin.RemoveAnchor(ctx, adminWs, "joiner-b")
	if err != nil {
		t.Fatalf("RemoveAnchor: %v", err)
	}

	// A's roster loop must adopt the new key.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if k, ok := dA.currentKey(aWs); ok && k == newKey {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gotA, ok := dA.currentKey(aWs)
	if !ok || gotA != newKey {
		t.Fatalf("joiner-A did not adopt new key after B was removed (oldKey=%x newKey=%x gotA=%x)", oldKey, newKey, gotA)
	}

	// A sends a post-removal message; admin must decrypt it.
	if err := dA.Send(ctx, "joiner-a", SendArgs{Subject: "still-works", Body: "A post-remove msg"}); err != nil {
		t.Fatalf("A post-remove Send: %v", err)
	}
	eA := extractLastEnvelope(t, fake, "out-joiner-a")
	open := adminReceiverFor(dAdmin, adminWs, "admin-recv-e")
	got, ok := open(eA)
	if !ok {
		t.Fatal("admin could not open A's post-remove message")
	}
	if got.Body != "A post-remove msg" {
		t.Fatalf("body mismatch: %q", got.Body)
	}

	// B did NOT receive a rekey grant at the new epoch → verify on roster.
	var bGotRekey bool
	for _, msg := range fake.Published(adminWs.Roster) {
		if len(msg) == 0 || msg[0] != '{' {
			continue
		}
		g, perr := parseAdmitGrant([]byte(msg))
		if perr == nil && g.AnchorID == "joiner-b" && g.Epoch == adminWs.Epoch {
			bGotRekey = true
		}
	}
	if bGotRekey {
		t.Fatal("evicted joiner-B must NOT receive a rekey grant at the new epoch")
	}

	// B cannot unwrap the new key from any grant on the roster channel.
	for _, msg := range fake.Published(adminWs.Roster) {
		if len(msg) == 0 || msg[0] != '{' {
			continue
		}
		g, perr := parseAdmitGrant([]byte(msg))
		if perr != nil || g.AnchorID != "joiner-b" {
			continue
		}
		k, _, ok := ProcessRekey(g, bEncPriv[:], adminWs.AdminPub)
		if ok && k == newKey {
			t.Fatal("evicted joiner-B must not derive the new epoch key")
		}
	}

	// B with old epoch key cannot open A's new-epoch message.
	// Build a minimal B daemon holding only the old key.
	bOldState := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: adminWs.RootID},
			{ID: "joiner-b", Parent: adminWs.RootID, SignSeed: make([]byte, ed25519.SeedSize), EncPriv: bEncPriv[:]},
		},
		Workspaces: []agentstate.Workspace{{
			RootID:   adminWs.RootID,
			E2E:      true,
			Epoch:    aWs.Epoch - 1, // old epoch
			Key:      oldKey[:],
			AdminPub: append([]byte(nil), adminWs.AdminPub...),
		}},
	}
	dBOld := &Daemon{state: bOldState, trust: newTrustGraph(), replay: newReplayWindow()}
	dBOld.trust.anchors = dAdmin.trust.anchors
	_, bOk := dBOld.openerFor("joiner-b")(eA)
	if bOk {
		t.Fatal("evicted joiner-B with old epoch key must not decrypt A's new-epoch message")
	}

	// Relay-blind: new key bytes must not appear in cleartext in any published frame.
	allChannels := []string{"out-joiner-a", "out-joiner-b", adminWs.Roster, adminWs.WaitingRoom}
	relayBlindCheck(t, fake, allChannels, newKey[:], "")
}
