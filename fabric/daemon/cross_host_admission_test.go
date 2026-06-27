package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/e2e"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

func TestCrossHostJoinAdmitConverge(t *testing.T) {
	ctx := context.Background()

	// --- Admin setup ---
	var wsKey [32]byte
	rand.Read(wsKey[:])

	adminPub, adminPrivSeed, _ := ed25519.GenerateKey(rand.Reader)
	// adminPriv seed is adminPrivSeed.Seed() — that's what workspace.go stores.

	// Admin root agent.
	adminRootPub, adminRootPriv, _ := ed25519.GenerateKey(rand.Reader)

	fake := hubclient.NewFake()

	// Admin daemon state: root agent "admin-root", one child "admin-agent".
	adminState := &agentstate.State{
		Agents: []agentstate.Agent{
			{ID: "admin-root"},
			{ID: "admin-agent", Parent: "admin-root", SignSeed: adminRootPriv.Seed()},
		},
		Workspaces: []agentstate.Workspace{{
			RootID:      "admin-root",
			E2E:         true,
			Epoch:       1,
			Key:         wsKey[:],
			AdminPub:    adminPub,
			AdminPriv:   adminPrivSeed.Seed(),
			Roster:      "roster",
			WaitingRoom: "wroom",
		}},
	}

	dAdmin := &Daemon{
		state:  adminState,
		hub:    fake,
		trust:  newTrustGraph(),
		replay: newReplayWindow(),
	}

	// Seed the admin root agent as an anchor (it owns the workspace).
	dAdmin.trust.anchors.set("admin-root", adminRootPub)

	ws := &adminState.Workspaces[0]

	// --- Joiner setup ---
	joinerSignPub, joinerSignPriv, _ := ed25519.GenerateKey(rand.Reader)
	joinerEncPub, joinerEncPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	req := JoinRequest{
		ReqID:   "joiner-1",
		Name:    "joiner",
		SignPub: joinerSignPub,
		EncPub:  joinerEncPub[:],
	}

	// --- Admin admits joiner ---
	grant, err := dAdmin.AdmitJoinRequest(ctx, ws, req)
	if err != nil {
		t.Fatalf("AdmitJoinRequest: %v", err)
	}

	// --- Relay-blind assertion on waiting room ---
	wroomMsgs := fake.Published("wroom")
	if len(wroomMsgs) == 0 {
		t.Fatal("no message published to waiting room")
	}
	grantRaw := wroomMsgs[0]
	// Parse it back to verify WrappedKey is non-empty.
	parsedGrant, err := parseAdmitGrant([]byte(grantRaw))
	if err != nil {
		t.Fatalf("parseAdmitGrant: %v", err)
	}
	if len(parsedGrant.WrappedKey) == 0 {
		t.Fatal("WrappedKey must be non-empty in published grant")
	}
	// The raw wire frame must not contain the plaintext workspace key — neither
	// as raw bytes nor as the base64 form JSON would actually emit for a []byte.
	if bytes.Contains([]byte(grantRaw), wsKey[:]) {
		t.Fatal("relay-blind violation: plaintext workspace key (raw) found in grant frame")
	}
	if strings.Contains(grantRaw, base64.StdEncoding.EncodeToString(wsKey[:])) {
		t.Fatal("relay-blind violation: base64 workspace key found in grant frame")
	}

	// --- Joiner processes grant ---
	joinerWs, recoveredKey, ok := ProcessAdmitGrant(grant, joinerEncPriv[:])
	if !ok {
		t.Fatal("ProcessAdmitGrant failed")
	}
	if recoveredKey != wsKey {
		t.Fatalf("key mismatch: got %x want %x", recoveredKey, wsKey)
	}
	if joinerWs.RootID != ws.RootID {
		t.Fatalf("RootID mismatch: got %q want %q", joinerWs.RootID, ws.RootID)
	}

	// --- Convergence: joiner seals a message, admin opens it ---
	// After admit, "joiner-1" is in admin's anchor set.
	joinerEnv, err := e2e.SealMessage(wsKey, 1, ws.RootID, "joiner-1", joinerSignPriv, 1, encodeContent("hi", "secret body"))
	if err != nil {
		t.Fatalf("SealMessage: %v", err)
	}
	wrapped := envelope.Envelope{
		V:    1,
		ID:   "m-joiner",
		From: "joiner-1",
		Kind: envelope.KindChat,
		Enc:  base64.StdEncoding.EncodeToString(joinerEnv.Marshal()),
	}

	// admin-agent is the local receiver (it's in the admin's workspace).
	got, ok := dAdmin.openerFor("admin-agent")(wrapped)
	if !ok {
		t.Fatal("admin opener must accept admitted joiner's message")
	}
	if got.Body != "secret body" {
		t.Fatalf("body mismatch: got %q want %q", got.Body, "secret body")
	}

	// --- Reject: intruder (never admitted) must be dropped ---
	intruderPub, intruderPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = intruderPub
	intruderEnv, err := e2e.SealMessage(wsKey, 1, ws.RootID, "intruder", intruderPriv, 1, encodeContent("evil", "evil body"))
	if err != nil {
		t.Fatalf("SealMessage intruder: %v", err)
	}
	intruderWrapped := envelope.Envelope{
		V:    1,
		ID:   "m-intruder",
		From: "intruder",
		Kind: envelope.KindChat,
		Enc:  base64.StdEncoding.EncodeToString(intruderEnv.Marshal()),
	}
	if _, ok := dAdmin.openerFor("admin-agent")(intruderWrapped); ok {
		t.Fatal("admin opener must drop intruder (never admitted)")
	}

	// --- Roster channel assertion ---
	rosterMsgs := fake.Published("roster")
	if len(rosterMsgs) == 0 {
		t.Fatal("no message published to roster channel")
	}
	rosterFrame, err := openRosterFrame(wsKey, rosterMsgs[0])
	if err != nil {
		t.Fatalf("openRosterFrame: %v", err)
	}
	if rosterFrame.Kind != "anchors" {
		t.Fatalf("roster frame kind = %q, want %q", rosterFrame.Kind, "anchors")
	}
	// The anchor blob must name "joiner-1".
	var anchorSet struct {
		Devices [][]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(rosterFrame.AnchorBlob, &anchorSet); err != nil {
		t.Fatalf("unmarshal anchorBlob: %v", err)
	}
	foundJoiner, foundAdmin := false, false
	for _, pair := range anchorSet.Devices {
		if len(pair) != 2 {
			continue
		}
		var id string
		if err := json.Unmarshal(pair[0], &id); err == nil {
			switch id {
			case "joiner-1":
				foundJoiner = true
			case "admin-root":
				foundAdmin = true
			}
		}
	}
	if !foundJoiner {
		t.Fatal("anchor blob must include joiner-1")
	}
	// Admitting the joiner must NOT wipe the prior anchor (admin-root).
	if !foundAdmin {
		t.Fatal("anchor blob must still include the prior anchor admin-root after admit")
	}
	// Verify admin signature on the anchor blob.
	if !ed25519.Verify(adminPub, rosterFrame.AnchorBlob, rosterFrame.AnchorSig) {
		t.Fatal("anchor blob signature invalid")
	}
}
