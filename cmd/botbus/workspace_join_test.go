package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

// TestWorkspaceJoinCompletes exercises the full join flow:
// 1. workspaceJoin posts a JoinRequest to the waiting room.
// 2. Test code acts as admin: extracts EncPub+ReqID, wraps workspace key,
//    signs and publishes AdmitGrant.
// 3. workspaceJoin processes the grant and saves state.
func TestWorkspaceJoinCompletes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	fake := hubclient.NewFake()
	wroom := "test-wroom"
	mintN := 0
	d := hostagent.Deps{
		Hub:       fake,
		StatePath: statePath,
		MintKey:   func() string { mintN++; return fmt.Sprintf("minted-key-%d", mintN) },
	}

	// Admin keypair + workspace key.
	var wsKey [32]byte
	if _, err := rand.Read(wsKey[:]); err != nil {
		t.Fatalf("rand.Read wsKey: %v", err)
	}
	adminPub, adminPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	// Run workspaceJoin in a goroutine; it blocks waiting for the grant.
	joinErr := make(chan error, 1)
	go func() {
		joinErr <- workspaceJoin(ctx, d, wroom, "bob-laptop")
	}()

	// Poll until the join request is published.
	var reqID string
	var encPubBytes []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		msgs := fake.Published(wroom)
		for _, msg := range msgs {
			var jr daemon.JoinRequest
			if err := json.Unmarshal([]byte(msg), &jr); err != nil {
				continue
			}
			if jr.ReqID != "" {
				reqID = jr.ReqID
				encPubBytes = jr.EncPub
				goto found
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for JoinRequest")
found:

	// Admin side: wrap workspace key to joiner's EncPub, sign, publish grant.
	var encPub [32]byte
	copy(encPub[:], encPubBytes)
	wrapped, err := box.SealAnonymous(nil, wsKey[:], &encPub, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	grant := daemon.AdmitGrant{
		ReqID:       reqID,
		AnchorID:    reqID,
		RootID:      "remote-root",
		Epoch:       1,
		WrappedKey:  wrapped,
		AdminPub:    []byte(adminPub),
		Roster:      "remote-roster",
		WaitingRoom: wroom,
	}
	grant.Sig = ed25519.Sign(adminPriv, daemon.GrantSignedPayload(grant))
	gb, err := json.Marshal(grant)
	if err != nil {
		t.Fatalf("json.Marshal grant: %v", err)
	}
	if err := fake.Publish(ctx, wroom, string(gb)); err != nil {
		t.Fatalf("fake.Publish grant: %v", err)
	}

	// Wait for workspaceJoin to complete.
	select {
	case err := <-joinErr:
		if err != nil {
			t.Fatalf("workspaceJoin: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workspaceJoin timed out")
	}

	// Verify saved state.
	st, err := agentstate.Load(statePath)
	if err != nil {
		t.Fatalf("agentstate.Load: %v", err)
	}

	// Workspace must be present with correct fields.
	var ws *agentstate.Workspace
	for i := range st.Workspaces {
		if st.Workspaces[i].RootID == "remote-root" {
			ws = &st.Workspaces[i]
			break
		}
	}
	if ws == nil {
		t.Fatal("workspace not saved in state")
	}
	var gotKey [32]byte
	copy(gotKey[:], ws.Key)
	if gotKey != wsKey {
		t.Fatalf("workspace Key mismatch: got %x want %x", gotKey, wsKey)
	}
	if string(ws.AdminPub) != string(adminPub) {
		t.Fatalf("workspace AdminPub mismatch: got %x want %x", ws.AdminPub, []byte(adminPub))
	}

	// Local agent must be present with ID == reqID and EncPriv set.
	var ag *agentstate.Agent
	for i := range st.Agents {
		if st.Agents[i].ID == reqID {
			ag = &st.Agents[i]
			break
		}
	}
	if ag == nil {
		t.Fatalf("local agent with ID %q not saved in state", reqID)
	}
	if len(ag.EncPriv) != 32 {
		t.Fatalf("agent EncPriv length = %d, want 32", len(ag.EncPriv))
	}
}

// TestResolveWaitingRoom verifies target→channel-id extraction.
func TestResolveWaitingRoom(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"abc123", "abc123"},
		{"https://abc123.lab.space/", "abc123"},
		{"https://abc123.llama.space", "abc123"},
		{"abc123.example.com", "abc123"},
		{"abc123/path", "abc123"},
	}
	for _, tc := range cases {
		if got := resolveWaitingRoom(tc.input); got != tc.want {
			t.Errorf("resolveWaitingRoom(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
