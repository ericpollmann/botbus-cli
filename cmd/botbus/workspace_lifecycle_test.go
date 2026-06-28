package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

// buildAdminStateWithAnchors builds an e2e workspace state at epoch 1 with
// the given anchors already admitted and saves it to statePath. Returns the
// encPriv key for each anchor (keyed by anchorID).
func buildAdminStateWithAnchors(t *testing.T, statePath string, anchorIDs ...string) map[string]*[32]byte {
	t.Helper()

	var wsKey [32]byte
	if _, err := rand.Read(wsKey[:]); err != nil {
		t.Fatalf("rand.Read wsKey: %v", err)
	}
	adminPub, adminPrivSeed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	encPrivs := make(map[string]*[32]byte, len(anchorIDs))
	anchors := make([]agentstate.AnchorRef, 0, len(anchorIDs))
	for _, id := range anchorIDs {
		encPub, encPriv, err := box.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("box.GenerateKey for %s: %v", id, err)
		}
		var signPub [32]byte
		if _, err := rand.Read(signPub[:]); err != nil {
			t.Fatalf("rand.Read signPub for %s: %v", id, err)
		}
		encPrivs[id] = encPriv
		anchors = append(anchors, agentstate.AnchorRef{
			ID:      id,
			SignPub: signPub[:],
			EncPub:  encPub[:],
		})
	}

	st := &agentstate.State{
		ActiveWorkspace: "admin-root",
		Agents:          []agentstate.Agent{{ID: "admin-root", Name: "admin-root"}},
		Workspaces: []agentstate.Workspace{{
			RootID:    "admin-root",
			E2E:       true,
			Epoch:     1,
			Key:       wsKey[:],
			AdminPub:  []byte(adminPub),
			AdminPriv: adminPrivSeed.Seed(),
			Roster:    "roster-chan",
			Anchors:   anchors,
		}},
	}
	if err := agentstate.Save(statePath, st); err != nil {
		t.Fatalf("agentstate.Save: %v", err)
	}
	return encPrivs
}

// TestWorkspaceKeyRotateBumpsEpoch verifies that workspaceKeyRotate:
//   (a) bumps the workspace epoch to 2 and changes the key,
//   (b) publishes a signed rekey grant for the surviving anchor to the roster,
//   (c) the anchor's encPriv can ProcessRekey the grant to obtain the new key.
func TestWorkspaceKeyRotateBumpsEpoch(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	encPrivs := buildAdminStateWithAnchors(t, statePath, "anchor-1")

	fake := hubclient.NewFake()
	d := hostagent.Deps{
		Hub:       fake,
		StatePath: statePath,
		MintKey:   func() string { return "unused" },
	}

	ctx := context.Background()
	if err := workspaceKeyRotate(ctx, d, ""); err != nil {
		t.Fatalf("workspaceKeyRotate: %v", err)
	}

	// Reload from disk to prove persistence.
	st, err := agentstate.Load(statePath)
	if err != nil {
		t.Fatalf("agentstate.Load: %v", err)
	}
	if len(st.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(st.Workspaces))
	}
	ws := st.Workspaces[0]

	// (a) Epoch bumped and key changed.
	if ws.Epoch != 2 {
		t.Fatalf("epoch: got %d, want 2", ws.Epoch)
	}

	// (b) A rekey grant for anchor-1 was published to the roster channel.
	rosterMsgs := fake.Published("roster-chan")
	if len(rosterMsgs) == 0 {
		t.Fatal("no messages published to roster channel")
	}
	var rekeyGrant *daemon.AdmitGrant
	for _, msg := range rosterMsgs {
		if len(msg) == 0 || msg[0] != '{' {
			continue
		}
		var g daemon.AdmitGrant
		if err := json.Unmarshal([]byte(msg), &g); err != nil {
			continue
		}
		if g.AnchorID != "anchor-1" {
			continue
		}
		cp := g
		rekeyGrant = &cp
		break
	}
	if rekeyGrant == nil {
		t.Fatal("no rekey grant for anchor-1 found on roster channel")
	}

	// (c) The anchor can unwrap the new key.
	encPriv := encPrivs["anchor-1"]
	adminPub := ws.AdminPub
	recovered, _, ok := daemon.ProcessRekey(*rekeyGrant, encPriv[:], adminPub)
	if !ok {
		t.Fatal("anchor-1 could not ProcessRekey the rekey grant")
	}
	if recovered != [32]byte(ws.Key) {
		t.Fatalf("unwrapped key mismatch: got %x, want %x", recovered, ws.Key)
	}
}

// TestWorkspaceRemoveEvictsAndRotates verifies that workspaceRemove:
//   (a) removes the specified anchor from ws.Anchors,
//   (b) bumps the epoch,
//   (c) publishes a rekey grant for the surviving anchor but NOT for the removed one.
func TestWorkspaceRemoveEvictsAndRotates(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	buildAdminStateWithAnchors(t, statePath, "anchor-A", "anchor-B")

	fake := hubclient.NewFake()
	d := hostagent.Deps{
		Hub:       fake,
		StatePath: statePath,
		MintKey:   func() string { return "unused" },
	}

	ctx := context.Background()
	if err := workspaceRemove(ctx, d, "", "anchor-B"); err != nil {
		t.Fatalf("workspaceRemove: %v", err)
	}

	// Reload from disk to prove persistence.
	st, err := agentstate.Load(statePath)
	if err != nil {
		t.Fatalf("agentstate.Load: %v", err)
	}
	ws := st.Workspaces[0]

	// (a) anchor-B must be gone from ws.Anchors; anchor-A must remain.
	for _, ar := range ws.Anchors {
		if ar.ID == "anchor-B" {
			t.Fatal("anchor-B must be absent from ws.Anchors after removal")
		}
	}
	var foundA bool
	for _, ar := range ws.Anchors {
		if ar.ID == "anchor-A" {
			foundA = true
		}
	}
	if !foundA {
		t.Fatal("anchor-A must still be in ws.Anchors after removing anchor-B")
	}

	// (b) Epoch bumped.
	if ws.Epoch <= 1 {
		t.Fatalf("epoch did not bump: got %d, want > 1", ws.Epoch)
	}

	// (c) Rekey grant published for anchor-A but NOT for anchor-B.
	rosterMsgs := fake.Published("roster-chan")
	var foundGrantA, foundGrantB bool
	for _, msg := range rosterMsgs {
		if len(msg) == 0 || msg[0] != '{' {
			continue
		}
		var g daemon.AdmitGrant
		if err := json.Unmarshal([]byte(msg), &g); err != nil {
			continue
		}
		if g.AnchorID == "anchor-A" {
			foundGrantA = true
		}
		if g.AnchorID == "anchor-B" {
			foundGrantB = true
		}
	}
	if !foundGrantA {
		t.Fatal("surviving anchor-A must receive a rekey grant")
	}
	if foundGrantB {
		t.Fatal("removed anchor-B must NOT receive a rekey grant")
	}
}
