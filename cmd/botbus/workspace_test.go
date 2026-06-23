package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// stubWorkspaceControl mints a fresh id per call and accepts any
// Bearer-authenticated register. Mirrors the hostagent test stub; the real
// auth/HMAC validation is exercised in the router's own tests. We never touch
// the live router here.
func stubWorkspaceControl(t *testing.T) *control.Client {
	t.Helper()
	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mint", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("minted-%d", n.Add(1))})
	})
	mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return control.NewClient(srv.URL)
}

// fakeDeps returns a hostagent.Deps wired entirely to in-memory/test fakes and a
// temp state path under t.TempDir(). It NEVER reads ~/.botbus or the live hub /
// router.
func fakeDeps(t *testing.T) hostagent.Deps {
	t.Helper()
	return hostagent.Deps{
		Hub:       hubclient.NewFake(),
		Control:   stubWorkspaceControl(t),
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		MintKey:   func() string { return "key-fixed" },
	}
}

// workspaceCreate persists an org-root: an agent with no Parent.
func TestWorkspaceCreatePersistsOrgRoot(t *testing.T) {
	deps := fakeDeps(t)
	root, err := workspaceCreate(context.Background(), deps, "acme")
	if err != nil {
		t.Fatalf("workspaceCreate: %v", err)
	}
	if root.Name != "acme" {
		t.Fatalf("name = %q, want acme", root.Name)
	}
	if root.Parent != "" {
		t.Fatalf("org-root must have no parent, got %q", root.Parent)
	}
	if root.ID == "" || root.InboxChannel == "" {
		t.Fatalf("org-root not fully minted: %+v", root)
	}

	got, ok, err := hostagent.GetByName(deps.StatePath, "acme")
	if err != nil || !ok {
		t.Fatalf("GetByName acme: ok=%v err=%v", ok, err)
	}
	if got.ID != root.ID || got.Parent != "" {
		t.Fatalf("persisted org-root mismatch: %+v", got)
	}
}

// workspaceInvite finds the org-root by name and creates a member parented to
// it, returning a join URL that contains the member's inbox + ?user=<user>.
func TestWorkspaceInviteCreatesMemberUnderRoot(t *testing.T) {
	deps := fakeDeps(t)
	root, err := workspaceCreate(context.Background(), deps, "acme")
	if err != nil {
		t.Fatalf("workspaceCreate: %v", err)
	}

	joinURL, err := workspaceInvite(context.Background(), deps, "alice", "acme")
	if err != nil {
		t.Fatalf("workspaceInvite: %v", err)
	}

	member, ok, err := hostagent.GetByName(deps.StatePath, "alice")
	if err != nil || !ok {
		t.Fatalf("GetByName alice: ok=%v err=%v", ok, err)
	}
	if member.Parent != root.ID {
		t.Fatalf("member parent = %q, want org-root id %q", member.Parent, root.ID)
	}

	if !strings.Contains(joinURL, member.InboxChannel) {
		t.Fatalf("join URL %q should contain member inbox %q", joinURL, member.InboxChannel)
	}
	if !strings.Contains(joinURL, "?user=alice") {
		t.Fatalf("join URL %q should carry ?user=alice", joinURL)
	}
	want := fmt.Sprintf("https://%s.%s/?user=alice", member.InboxChannel, domain)
	if joinURL != want {
		t.Fatalf("join URL = %q, want %q", joinURL, want)
	}
}

// A user containing characters needing escaping (space) must be url-escaped in
// the join URL's query.
func TestWorkspaceInviteEscapesUser(t *testing.T) {
	deps := fakeDeps(t)
	if _, err := workspaceCreate(context.Background(), deps, "acme"); err != nil {
		t.Fatalf("workspaceCreate: %v", err)
	}
	joinURL, err := workspaceInvite(context.Background(), deps, "a b", "acme")
	if err != nil {
		t.Fatalf("workspaceInvite: %v", err)
	}
	if !strings.Contains(joinURL, "user=a%20b") && !strings.Contains(joinURL, "user=a+b") {
		t.Fatalf("join URL %q should url-escape the user", joinURL)
	}
	if strings.Contains(joinURL, "user=a b") {
		t.Fatalf("join URL %q must not contain a raw space in the query", joinURL)
	}
}

// Inviting into a workspace that doesn't exist errors clearly (and creates
// nothing).
func TestWorkspaceInviteMissingWorkspaceErrors(t *testing.T) {
	deps := fakeDeps(t)
	_, err := workspaceInvite(context.Background(), deps, "alice", "ghost")
	if err == nil {
		t.Fatal("inviting into a missing workspace should error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q should name the missing workspace", err)
	}
	if _, ok, _ := hostagent.GetByName(deps.StatePath, "alice"); ok {
		t.Fatal("no member should be created when the workspace is missing")
	}
}

// workspace list (via hostagent.List) returns the org-root + every member.
func TestWorkspaceListReturnsRootAndMembers(t *testing.T) {
	deps := fakeDeps(t)
	if _, err := workspaceCreate(context.Background(), deps, "acme"); err != nil {
		t.Fatalf("workspaceCreate: %v", err)
	}
	if _, err := workspaceInvite(context.Background(), deps, "alice", "acme"); err != nil {
		t.Fatalf("workspaceInvite: %v", err)
	}

	agents, err := hostagent.List(deps.StatePath)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected org-root + 1 member, got %d: %+v", len(agents), agents)
	}
	byName := map[string]agentstate.Agent{}
	for _, a := range agents {
		byName[a.Name] = a
	}
	if byName["acme"].Parent != "" {
		t.Fatalf("acme should be the org-root (no parent): %+v", byName["acme"])
	}
	if byName["alice"].Parent != byName["acme"].ID {
		t.Fatalf("alice should be parented to acme: %+v", byName["alice"])
	}
}

// parseInviteArgs must accept the --workspace flag in ANY position relative to
// the positional user. The bug this guards: `invite ethan --workspace x` left
// --workspace unparsed (flag.Parse stops at the first positional), so the
// documented form fell through to usage.
func TestParseInviteArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		user, ws string
		ok       bool
	}{
		{"positional then flag (the bug)", []string{"ethan", "--workspace", "mythwork"}, "ethan", "mythwork", true},
		{"flag then positional", []string{"--workspace", "mythwork", "ethan"}, "ethan", "mythwork", true},
		{"flag=value then positional", []string{"--workspace=mythwork", "ethan"}, "ethan", "mythwork", true},
		{"positional then flag=value", []string{"ethan", "--workspace=mythwork"}, "ethan", "mythwork", true},
		{"missing workspace", []string{"ethan"}, "", "", false},
		{"missing user", []string{"--workspace", "mythwork"}, "", "", false},
		{"empty", nil, "", "", false},
		{"extra positional rejected", []string{"ethan", "bob", "--workspace", "x"}, "", "", false},
		{"unknown flag rejected", []string{"ethan", "--bogus"}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user, ws, ok := parseInviteArgs(c.args)
			if ok != c.ok || user != c.user || ws != c.ws {
				t.Fatalf("parseInviteArgs(%q) = (%q,%q,%v), want (%q,%q,%v)",
					c.args, user, ws, ok, c.user, c.ws, c.ok)
			}
		})
	}
}
