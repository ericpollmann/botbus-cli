package agentstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetCursorPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}
	if err := Save(path, s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := SetCursor(path, "a", "cursor-9"); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	reloaded, _ := Load(path)
	got, _ := reloaded.Get("a")
	if got.Cursor != "cursor-9" {
		t.Fatalf("cursor = %q, want cursor-9", got.Cursor)
	}
	if err := SetCursor(path, "missing", "x"); err == nil {
		t.Fatal("SetCursor on unknown id should error")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Run("BOTBUS_STATE override wins", func(t *testing.T) {
		t.Setenv("BOTBUS_STATE", "/custom/state.json")
		if got := DefaultPath(); got != "/custom/state.json" {
			t.Fatalf("DefaultPath = %q, want /custom/state.json", got)
		}
	})

	t.Run("falls back to home dir when unset", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("BOTBUS_STATE", "")
		t.Setenv("HOME", home)
		want := filepath.Join(home, ".botbus", "state.json")
		if got := DefaultPath(); got != want {
			t.Fatalf("DefaultPath = %q, want %q", got, want)
		}
	})

	t.Run("falls back to relative path when home is unknown", func(t *testing.T) {
		t.Setenv("BOTBUS_STATE", "")
		t.Setenv("HOME", "") // makes os.UserHomeDir error on unix
		if got := DefaultPath(); got != ".botbus/state.json" {
			t.Fatalf("DefaultPath = %q, want .botbus/state.json", got)
		}
	})
}

func TestLoadReadErrorNotIsNotExist(t *testing.T) {
	// A directory exists but cannot be read as a file: os.ReadFile returns an
	// error that is *not* IsNotExist, so Load must surface it.
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("Load on a directory path should error")
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load on malformed JSON should error")
	}
}

func TestSetCursorLoadError(t *testing.T) {
	// A directory path makes the internal Load fail, so SetCursor must return
	// the load error before attempting any save.
	dir := t.TempDir()
	if err := SetCursor(dir, "a", "cursor-1"); err == nil {
		t.Fatal("SetCursor with an unreadable path should error")
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if len(s.Agents) != 0 {
		t.Fatalf("expected empty state, got %d agents", len(s.Agents))
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := &State{
		Daemon: Daemon{RouterURL: "http://127.0.0.1:8090", HubBase: "https://botbus.ai", HubDomain: "botbus.ai"},
		Agents: []Agent{{
			ID: "myth-compiler", Key: "key-aaa", Name: "compiler",
			InboxChannel: "inbox-1", Focus: "packages/compile", Mode: "session",
			BatchMS: 3000, BatchN: 5, BatchBytes: 20480, ModelTier: "opus",
		}},
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents[0].ID != "myth-compiler" || got.Agents[0].Key != "key-aaa" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Daemon.RouterURL != want.Daemon.RouterURL {
		t.Fatalf("daemon config lost: %+v", got.Daemon)
	}
}

func TestSaveUsesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, &State{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file perm = %o, want 600", perm)
	}
}

func TestUpsertGetRemove(t *testing.T) {
	s := &State{}

	s.Upsert(Agent{ID: "a", InboxChannel: "i-a", Focus: "one"})
	s.Upsert(Agent{ID: "b", InboxChannel: "i-b"})
	if len(s.Agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(s.Agents))
	}

	s.Upsert(Agent{ID: "a", InboxChannel: "i-a", Focus: "two"})
	if len(s.Agents) != 2 {
		t.Fatalf("upsert duplicated id: %d agents", len(s.Agents))
	}
	got, ok := s.Get("a")
	if !ok || got.Focus != "two" {
		t.Fatalf("Get after upsert = %+v, ok=%v", got, ok)
	}

	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get missing should be false")
	}

	if !s.Remove("a") {
		t.Fatal("Remove existing should be true")
	}
	if s.Remove("a") {
		t.Fatal("Remove already-gone should be false")
	}
	if len(s.Agents) != 1 || s.Agents[0].ID != "b" {
		t.Fatalf("after remove, agents = %+v", s.Agents)
	}
}
