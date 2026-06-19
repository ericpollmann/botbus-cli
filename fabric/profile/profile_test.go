package profile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	p := &Profile{
		User:    "eric",
		Framing: "prefers this channel over the Claude Code desktop UI",
		Root:    Root{ID: "root-id", InboxChannel: "inbox-root", Key: "key-root"},
	}
	if err := Save(path, p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.User != "eric" || got.Root.ID != "root-id" || got.Root.Key != "key-root" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestLoadMissingReportsNotConfigured(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("Load missing should not error: %v", err)
	}
	if p.Configured() {
		t.Fatal("an absent profile must report Configured()==false")
	}
}

func TestSaveCreatesDirAnd0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "profile.json")
	if err := Save(path, &Profile{User: "x", Root: Root{ID: "r"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("BOTBUS_PROFILE", "/custom/profile.json")
	if got := DefaultPath(); got != "/custom/profile.json" {
		t.Fatalf("env override: got %q", got)
	}

	t.Setenv("BOTBUS_PROFILE", "")
	want := filepath.Join(mustHome(t), ".botbus", "profile.json")
	if got := DefaultPath(); got != want {
		t.Fatalf("home default: got %q want %q", got, want)
	}
}

func TestDefaultPathNoHome(t *testing.T) {
	// os.UserHomeDir resolves $HOME on unix; empty $HOME forces the error branch.
	if runtime.GOOS == "windows" {
		t.Skip("UserHomeDir uses different env vars on windows")
	}
	t.Setenv("BOTBUS_PROFILE", "")
	t.Setenv("HOME", "")
	if got := DefaultPath(); got != ".botbus/profile.json" {
		t.Fatalf("no-home fallback: got %q", got)
	}
}

// mustHome returns the home dir the test process resolves to, skipping if the
// platform has none (so the home-default assertion stays meaningful).
func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir on this platform: %v", err)
	}
	return home
}

func TestLoadReadError(t *testing.T) {
	// A directory cannot be read as a file: exercises the non-IsNotExist branch.
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("Load of a directory should error")
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load of malformed JSON should error")
	}
}

func TestSaveMkdirError(t *testing.T) {
	// Make the parent path a regular file so MkdirAll fails.
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Save(filepath.Join(blocker, "profile.json"), &Profile{}); err == nil {
		t.Fatal("Save should error when the parent dir cannot be created")
	}
}

func TestSaveWriteError(t *testing.T) {
	// Point the temp file at an unwritable location: the dir exists (MkdirAll is
	// a no-op on ".") but WriteFile of the .tmp into a read-only dir fails.
	roDir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(roDir, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	if err := Save(filepath.Join(roDir, "profile.json"), &Profile{}); err == nil {
		t.Fatal("Save should error when the .tmp file cannot be written")
	}
}

func TestSaveRenameError(t *testing.T) {
	// Pre-create a non-empty directory at the destination path: the .tmp write
	// succeeds, but renaming a file over a non-empty dir fails.
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed occupant: %v", err)
	}
	if err := Save(path, &Profile{}); err == nil {
		t.Fatal("Save should error when rename onto a non-empty dir fails")
	}
}
