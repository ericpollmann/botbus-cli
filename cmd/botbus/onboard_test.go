package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
)

func TestSeedSampleTaskPostsStartedFrame(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := seedSampleTask(context.Background(), srv.URL, "mythwork"); err != nil {
		t.Fatalf("seedSampleTask: %v", err)
	}
	if !strings.HasPrefix(gotBody, "mythwork: ") {
		t.Fatalf("frame should be sender-prefixed, got %q", gotBody)
	}
	for _, want := range []string{`"v":1`, `"type":"task.started"`, `"task":"onboarding"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("frame missing %q, got %q", want, gotBody)
		}
	}
}

func TestEnsureWorkspaceRootCreatesAndPersists(t *testing.T) {
	d := fakeDeps(t) // from workspace_test.go: fakes + temp state path
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	root, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("ensureWorkspaceRoot: %v", err)
	}
	if root.Name != "mythwork" || root.Parent != "" {
		t.Fatalf("org-root should be named mythwork with no parent, got %+v", root)
	}

	p, err := profile.Load(profilePath)
	if err != nil {
		t.Fatalf("profile.Load: %v", err)
	}
	if p.User != "eric" || p.Root.ID != root.ID || p.Root.InboxChannel != root.InboxChannel || p.Root.Key != root.Key {
		t.Fatalf("profile not persisted to the org-root: %+v", p)
	}
}

func TestEnsureWorkspaceRootReusesOnRerun(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	first, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-run should reuse the same org-root id, got %q then %q", first.ID, second.ID)
	}
	agents, _ := hostagent.List(d.StatePath)
	if len(agents) != 1 {
		t.Fatalf("re-run should not mint a second root, got %d agents", len(agents))
	}
}

func TestEnsureWorkspaceRootPreservesFraming(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	// Pre-seed a profile with an existing directive/framing.
	if err := profile.Save(profilePath, &profile.Profile{User: "eric", Framing: "ship fast"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err != nil {
		t.Fatalf("ensureWorkspaceRoot: %v", err)
	}
	p, err := profile.Load(profilePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.Framing != "ship fast" {
		t.Fatalf("existing Framing should be preserved, got %q", p.Framing)
	}
}

func TestEnsureWorkspaceRootSurfacesCorruptProfile(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(profilePath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed corrupt profile: %v", err)
	}
	if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err == nil {
		t.Fatal("a corrupt profile should surface an error, not be overwritten")
	}
	got, _ := os.ReadFile(profilePath)
	if string(got) != "{not valid json" {
		t.Fatalf("corrupt profile must not be overwritten, got %q", got)
	}
}
