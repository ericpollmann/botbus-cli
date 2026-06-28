package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

// buildAdminState builds an e2e workspace state with one pending join request
// and saves it to path. Returns the workspace's WaitingRoom channel id.
func buildAdminState(t *testing.T, path string) (waitingRoom string) {
	t.Helper()

	var wsKey [32]byte
	if _, err := rand.Read(wsKey[:]); err != nil {
		t.Fatalf("rand.Read wsKey: %v", err)
	}
	adminPub, adminPrivSeed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	var signPub [32]byte
	if _, err := rand.Read(signPub[:]); err != nil {
		t.Fatalf("rand.Read signPub: %v", err)
	}
	encPub, _, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("box.GenerateKey: %v", err)
	}

	st := &agentstate.State{
		ActiveWorkspace: "admin-root",
		Agents:          []agentstate.Agent{{ID: "admin-root", Name: "admin-root"}},
		Workspaces: []agentstate.Workspace{{
			RootID:      "admin-root",
			E2E:         true,
			Epoch:       1,
			Key:         wsKey[:],
			AdminPub:    []byte(adminPub),
			AdminPriv:   adminPrivSeed.Seed(),
			Roster:      "roster-chan",
			WaitingRoom: "wroom-chan",
			Pending: []agentstate.PendingJoin{{
				ReqID:   "req-001",
				Name:    "alice-laptop",
				SignPub: signPub[:],
				EncPub:  encPub[:],
			}},
		}},
	}
	if err := agentstate.Save(path, st); err != nil {
		t.Fatalf("agentstate.Save: %v", err)
	}
	return "wroom-chan"
}

// TestWorkspaceAdmit is the TDD test for workspaceAdmit:
//   (a) ws.Anchors gains the admitted anchor,
//   (b) ws.Pending is empty after admit,
//   (c) a grant was published to the waiting-room channel and parses as an
//       AdmitGrant with AnchorID == reqID.
func TestWorkspaceAdmit(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	waitingRoom := buildAdminState(t, statePath)

	fake := hubclient.NewFake()
	d := hostagent.Deps{
		Hub:       fake,
		StatePath: statePath,
		MintKey:   func() string { return "unused" },
	}

	ctx := context.Background()
	anchorCount, err := workspaceAdmit(ctx, d, "", "req-001")
	if err != nil {
		t.Fatalf("workspaceAdmit: %v", err)
	}
	if anchorCount != 1 {
		t.Fatalf("expected 1 anchor after admit, got %d", anchorCount)
	}

	// (a+b) Reload from disk and verify Anchors/Pending.
	st, err := agentstate.Load(statePath)
	if err != nil {
		t.Fatalf("agentstate.Load: %v", err)
	}
	if len(st.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(st.Workspaces))
	}
	ws := st.Workspaces[0]

	if len(ws.Pending) != 0 {
		t.Fatalf("ws.Pending should be empty after admit, got %d entries", len(ws.Pending))
	}
	found := false
	for _, ar := range ws.Anchors {
		if ar.ID == "req-001" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("admitted anchor req-001 not in ws.Anchors: %+v", ws.Anchors)
	}

	// (c) Grant was published to the waiting room.
	pubs := fake.Published(waitingRoom)
	if len(pubs) == 0 {
		t.Fatalf("no messages published to waiting-room channel %q", waitingRoom)
	}
	var grant daemon.AdmitGrant
	if err := json.Unmarshal([]byte(pubs[len(pubs)-1]), &grant); err != nil {
		t.Fatalf("parse published grant: %v", err)
	}
	if grant.AnchorID != "req-001" {
		t.Fatalf("grant.AnchorID = %q, want req-001", grant.AnchorID)
	}
}

// TestWorkspaceAdmitUnknownReqID verifies a clear error when reqID is not found.
func TestWorkspaceAdmitUnknownReqID(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	buildAdminState(t, statePath)

	fake := hubclient.NewFake()
	d := hostagent.Deps{
		Hub:       fake,
		StatePath: statePath,
	}

	ctx := context.Background()
	_, err := workspaceAdmit(ctx, d, "", "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown reqID, got nil")
	}
}

// TestWorkspaceAdmitNonE2ERejectsWithGuard verifies that workspaceAdmit returns
// a clear error when the resolved workspace is not end-to-end encrypted (M-4).
func TestWorkspaceAdmitNonE2ERejectsWithGuard(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	st := &agentstate.State{
		ActiveWorkspace: "plain-root",
		Agents:          []agentstate.Agent{{ID: "plain-root", Name: "plain-root"}},
		Workspaces: []agentstate.Workspace{{
			RootID: "plain-root",
			E2E:    false, // not encrypted
		}},
	}
	if err := agentstate.Save(statePath, st); err != nil {
		t.Fatalf("agentstate.Save: %v", err)
	}

	fake := hubclient.NewFake()
	d := hostagent.Deps{
		Hub:       fake,
		StatePath: statePath,
		MintKey:   func() string { return "unused" },
	}

	ctx := context.Background()
	_, err := workspaceAdmit(ctx, d, "", "any-req-id")
	if err == nil {
		t.Fatal("expected error for non-e2e workspace, got nil")
	}
	if !strings.Contains(err.Error(), "not end-to-end encrypted") {
		t.Fatalf("expected 'not end-to-end encrypted' in error, got: %v", err)
	}
}
