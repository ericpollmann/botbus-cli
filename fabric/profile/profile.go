// Package profile is the one-time local operator profile for the botbus
// console: who you are, the standing framing injected into agent welcomes, and
// your root channel's credentials. Stored mode 0600 next to the daemon state.
package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Root struct {
	ID           string `json:"id"`
	InboxChannel string `json:"inbox_channel"`
	Key          string `json:"key"`
}

type Profile struct {
	User    string `json:"user"`
	Framing string `json:"framing"`
	Root    Root   `json:"root"`
}

// Configured reports whether first-run setup has completed (a root exists).
func (p *Profile) Configured() bool { return p != nil && p.Root.ID != "" }

// DefaultPath returns $BOTBUS_PROFILE or ~/.botbus/profile.json.
func DefaultPath() string {
	if v := os.Getenv("BOTBUS_PROFILE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".botbus/profile.json"
	}
	return filepath.Join(home, ".botbus", "profile.json")
}

// Load reads the profile; a missing file yields an empty (unconfigured) Profile.
func Load(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Profile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Save writes the profile atomically (temp + rename), mode 0600.
func Save(path string, p *Profile) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
