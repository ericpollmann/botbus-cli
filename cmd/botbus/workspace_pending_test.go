package main

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

func TestWorkspacePendingLists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := &agentstate.State{
		ActiveWorkspace: "root1",
		Workspaces: []agentstate.Workspace{{
			RootID: "root1", E2E: true,
			Pending: []agentstate.PendingJoin{{ReqID: "r1", Name: "alice-laptop", SignPub: make([]byte, 32), EncPub: make([]byte, 32)}},
		}},
	}
	if err := agentstate.Save(path, st); err != nil {
		t.Fatal(err)
	}
	out, err := workspacePending(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`r1`).MatchString(out) || !regexp.MustCompile(`alice-laptop`).MatchString(out) {
		t.Fatalf("missing request fields: %q", out)
	}
	if !regexp.MustCompile(`[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}`).MatchString(out) {
		t.Fatalf("missing SAS fingerprint: %q", out)
	}
}
