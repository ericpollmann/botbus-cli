package daemon

// persist_race_test.go — regression test for I-1: concurrent unlocked marshal
// of d.state (data race on state.json). Run with -race to detect the bug
// (pre-fix) and confirm it is absent (post-fix).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/hubclient"
	"golang.org/x/crypto/nacl/box"
)

// TestPersistWorkspaceKeyNoRace drives applyRekey and recordPending concurrently
// while both call persistWorkspaceKey, which writes d.state under d.mu (post-fix).
// The test MUST pass under `go test -race`.
func TestPersistWorkspaceKeyNoRace(t *testing.T) {
	// Set up a real state path so persistWorkspaceKey actually writes.
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	var wsKey [32]byte
	if _, err := rand.Read(wsKey[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	adminPub, adminPrivSeed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}

	// Seed one anchor so the workspace is fully valid.
	signPub, _, _ := ed25519.GenerateKey(rand.Reader)
	encPub, _, _ := box.GenerateKey(rand.Reader)

	st := &agentstate.State{
		Agents: []agentstate.Agent{{ID: "admin-root"}},
		Workspaces: []agentstate.Workspace{{
			RootID:      "admin-root",
			E2E:         true,
			Epoch:       1,
			Key:         wsKey[:],
			AdminPub:    adminPub,
			AdminPriv:   adminPrivSeed.Seed(),
			Roster:      "roster",
			WaitingRoom: "wroom",
			Anchors: []agentstate.AnchorRef{{
				ID:      "anchor-1",
				SignPub: []byte(signPub),
				EncPub:  encPub[:],
			}},
		}},
	}
	// Write initial state so the file exists before concurrent writes.
	if err := agentstate.Save(statePath, st); err != nil {
		t.Fatalf("initial agentstate.Save: %v", err)
	}

	fake := hubclient.NewFake()
	d := &Daemon{
		state:         st,
		statePath:     statePath,
		hub:           fake,
		trust:         newTrustGraph(),
		replay:        newReplayWindow(),
		handlers:      make(map[string]http.Handler),
		cancels:       make(map[string]context.CancelFunc),
		rosterCancels: make(map[string]context.CancelFunc),
		wroomCancels:  make(map[string]context.CancelFunc),
	}
	ws := &st.Workspaces[0]

	const iters = 250
	var wg sync.WaitGroup

	// Goroutine 1: repeatedly call applyRekey (Lock → mutate ws.Key/Epoch → Unlock → persist).
	wg.Add(1)
	go func() {
		defer wg.Done()
		var key [32]byte
		for i := 0; i < iters; i++ {
			rand.Read(key[:]) //nolint:errcheck
			d.applyRekey(ws, key, uint32(i+2))
		}
	}()

	// Goroutine 2: repeatedly call recordPending (Lock → append ws.Pending → Unlock → persist).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			req := JoinRequest{
				ReqID:   fmt.Sprintf("req-%d", i), // unique per iteration
				Name:    "joiner",
				SignPub: []byte(signPub),
				EncPub:  encPub[:],
			}
			d.recordPending(ws, req)
		}
	}()

	wg.Wait()
}
