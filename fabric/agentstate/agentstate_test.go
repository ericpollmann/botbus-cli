package agentstate

import (
	"fmt"
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
		Daemon:          Daemon{RouterURL: "http://127.0.0.1:8090", HubBase: "https://botbus.ai", HubDomain: "botbus.ai"},
		ActiveWorkspace: "myth-compiler",
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
	if got.ActiveWorkspace != want.ActiveWorkspace {
		t.Fatalf("ActiveWorkspace lost: got %q want %q", got.ActiveWorkspace, want.ActiveWorkspace)
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

// TestSaveCreatesBackupOfPriorVersion verifies that overwriting an existing
// state file first copies the prior contents to state.json.bak, so an
// accidental wipe of the live file is recoverable from the backup.
func TestSaveCreatesBackupOfPriorVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	v1 := &State{Agents: []Agent{{ID: "a", Key: "k1", InboxChannel: "i"}}}
	v2 := &State{Agents: []Agent{
		{ID: "a", Key: "k1", InboxChannel: "i"},
		{ID: "b", Key: "k2", InboxChannel: "j"},
	}}
	if err := Save(path, v1); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if err := Save(path, v2); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	bak, err := Load(path + ".bak")
	if err != nil {
		t.Fatalf("load backup: %v", err)
	}
	if len(bak.Agents) != 1 || bak.Agents[0].ID != "a" || bak.Agents[0].Key != "k1" {
		t.Fatalf("backup should hold the prior version (v1), got %+v", bak.Agents)
	}
	cur, _ := Load(path)
	if len(cur.Agents) != 2 {
		t.Fatalf("current file should be v2, got %+v", cur.Agents)
	}
}

// TestSaveFirstWriteCreatesNoBackup verifies there is nothing to back up on the
// very first write, so no backup file is produced.
func TestSaveFirstWriteCreatesNoBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("first save must not create a backup; stat err = %v", err)
	}
}

// TestSaveRotatesBackupGenerations verifies that successive saves rotate a
// small, bounded number of backup generations and drop the oldest.
func TestSaveRotatesBackupGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for i := 1; i <= 5; i++ {
		s := &State{
			Daemon: Daemon{RouterURL: fmt.Sprintf("v%d", i)},
			Agents: []Agent{{ID: "a", InboxChannel: "i"}},
		}
		if err := Save(path, s); err != nil {
			t.Fatalf("save v%d: %v", i, err)
		}
	}

	// Three generations are retained; the live file is v5, so the backups hold
	// v4 (most recent), v3, and v2. v1 has aged out.
	for f, want := range map[string]string{
		path + ".bak":   "v4",
		path + ".bak.1": "v3",
		path + ".bak.2": "v2",
	} {
		got, err := Load(f)
		if err != nil {
			t.Fatalf("load %s: %v", f, err)
		}
		if got.Daemon.RouterURL != want {
			t.Fatalf("%s = %q, want %q", f, got.Daemon.RouterURL, want)
		}
	}
	if _, err := os.Stat(path + ".bak.3"); !os.IsNotExist(err) {
		t.Fatalf("oldest generation should be dropped; .bak.3 stat err = %v", err)
	}
	if cur, _ := Load(path); cur.Daemon.RouterURL != "v5" {
		t.Fatalf("live file = %q, want v5", cur.Daemon.RouterURL)
	}
}

// TestSaveBackupUsesOwnerOnlyPermissions verifies the backup is as locked-down
// as the live file (0600) — it contains the same agent capability keys.
func TestSaveBackupUsesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if err := Save(path, &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}, {ID: "b", InboxChannel: "j"}}}); err != nil {
		t.Fatalf("save v2: %v", err)
	}
	info, err := os.Stat(path + ".bak")
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("backup perm = %o, want 600 (it holds secret keys)", perm)
	}
}

// TestSaveReadExistingError verifies Save surfaces an error when the existing
// state path cannot be read (here it is a directory).
func TestSaveReadExistingError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := Save(path, &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}); err == nil {
		t.Fatal("Save should error when the existing state path is unreadable")
	}
}

// TestSaveMkdirError verifies Save surfaces an error when the parent directory
// cannot be created.
func TestSaveMkdirError(t *testing.T) {
	roParent := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(roParent, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roParent, 0o700) })
	path := filepath.Join(roParent, "sub", "state.json")
	if err := Save(path, &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}); err == nil {
		t.Fatal("Save should error when the parent dir cannot be created")
	}
}

// TestSaveWriteTmpError verifies Save surfaces an error when the temp file
// cannot be written (the directory is read-only).
func TestSaveWriteTmpError(t *testing.T) {
	roDir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	if err := Save(filepath.Join(roDir, "state.json"), &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}); err == nil {
		t.Fatal("Save should error when the .tmp file cannot be written")
	}
}

// TestSaveBackupRotateError verifies Save surfaces an error when an existing
// backup cannot be rotated (the directory becomes read-only between writes).
func TestSaveBackupRotateError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Save(path, &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}}}); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if err := Save(path, &State{Agents: []Agent{{ID: "a", InboxChannel: "i"}, {ID: "b", InboxChannel: "j"}}}); err != nil {
		t.Fatalf("save v2: %v", err) // creates the first .bak
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := Save(path, &State{Agents: []Agent{{ID: "c", InboxChannel: "k"}}}); err == nil {
		t.Fatal("Save should error when an existing backup cannot be rotated")
	}
}

// TestSaveRefusesEmptyOverNonEmpty verifies the guard: a downgraded/buggy
// binary that tries to write zero agents over a populated file is refused, and
// the live file is left untouched (no backup rotated either, since the guard
// fires first).
func TestSaveRefusesEmptyOverNonEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seed := &State{Agents: []Agent{{ID: "a", Key: "k1", InboxChannel: "i"}}}
	if err := Save(path, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Save(path, &State{}); err == nil {
		t.Fatal("Save of empty agents over a populated file must be refused")
	}
	cur, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cur.Agents) != 1 || cur.Agents[0].ID != "a" {
		t.Fatalf("refused save must leave the file intact, got %+v", cur.Agents)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("refused save must not rotate backups; .bak stat err = %v", err)
	}
}

// TestSaveAllowEmptyClobbersAndBacksUp verifies that the legitimate "removed
// the last agent" case is honored when AllowEmpty is passed, and that the
// cleared agents remain recoverable from the backup.
func TestSaveAllowEmptyClobbersAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seed := &State{Agents: []Agent{{ID: "a", Key: "k1", InboxChannel: "i"}}}
	if err := Save(path, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := Save(path, &State{}, AllowEmpty()); err != nil {
		t.Fatalf("AllowEmpty save should succeed: %v", err)
	}
	if cur, _ := Load(path); len(cur.Agents) != 0 {
		t.Fatalf("live file should be empty after an intentional clear, got %+v", cur.Agents)
	}
	bak, err := Load(path + ".bak")
	if err != nil {
		t.Fatalf("load backup: %v", err)
	}
	if len(bak.Agents) != 1 || bak.Agents[0].Key != "k1" {
		t.Fatalf("backup should hold the cleared agents, got %+v", bak.Agents)
	}
}

// TestSaveEmptyOnFreshPathAllowed verifies the guard only blocks emptying a
// populated file — writing an empty state where none existed is fine.
func TestSaveEmptyOnFreshPathAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, &State{}); err != nil {
		t.Fatalf("empty save to a fresh path should succeed: %v", err)
	}
	if cur, _ := Load(path); len(cur.Agents) != 0 {
		t.Fatalf("expected empty state, got %+v", cur.Agents)
	}
}

// TestSaveOverUnparseableExistingAllowed verifies the guard does not block when
// the prior agent count is unknowable (corrupt file), and that the corrupt
// bytes are still preserved as a backup.
func TestSaveOverUnparseableExistingAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}
	if err := Save(path, &State{}); err != nil {
		t.Fatalf("save over unparseable existing should not be blocked: %v", err)
	}
	raw, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(raw) != "{ this is not json" {
		t.Fatalf("backup should preserve the prior bytes verbatim, got %q", raw)
	}
}

func TestWorkspaceRootIDWalksParents(t *testing.T) {
	s := &State{Agents: []Agent{
		{ID: "root", Parent: ""},
		{ID: "mid", Parent: "root"},
		{ID: "leaf", Parent: "mid"},
	}}
	if got := s.WorkspaceRootID("leaf"); got != "root" {
		t.Fatalf("got %q want root", got)
	}
	if got := s.WorkspaceRootID("root"); got != "root" {
		t.Fatalf("self-root: got %q", got)
	}
	if got := s.WorkspaceRootID("ghost"); got != "" {
		t.Fatalf("unknown agent: got %q want empty", got)
	}
}

func TestWorkspaceRootIDCycleSafe(t *testing.T) {
	s := &State{Agents: []Agent{
		{ID: "a", Parent: "b"},
		{ID: "b", Parent: "a"},
	}}
	// must terminate (not loop forever) AND return "" on a cycle
	if got := s.WorkspaceRootID("a"); got != "" {
		t.Fatalf("cycle must yield empty root, got %q", got)
	}
}

func TestWorkspaceForLooksUpKey(t *testing.T) {
	s := &State{
		Agents:     []Agent{{ID: "root", Parent: ""}, {ID: "leaf", Parent: "root"}},
		Workspaces: []Workspace{{RootID: "root", E2E: true, Key: []byte("k")}},
	}
	w, ok := s.WorkspaceFor("leaf")
	if !ok || !w.E2E {
		t.Fatalf("expected e2e workspace, got %v %v", w, ok)
	}
}
