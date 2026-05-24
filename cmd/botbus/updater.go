package main

// updater.go — best-effort self-update check at startup.
//
// At startup (before the TUI grabs stdin) we query proxy.golang.org for the
// latest pseudo-version of this module and compare it to the version embedded
// in this binary via runtime/debug.BuildInfo. If they differ, we prompt the
// user — y/Enter installs via `go install <pkg>@latest` and exits with a
// message; anything else proceeds with the current binary.
//
// Every failure path (devel build, no network, no `go` on PATH, proxy hiccup,
// install error) is silent or surfaces a one-line stderr note. The check
// never blocks the chat from running.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"time"
)

const (
	modulePath    = "github.com/ericpollmann/botbus-cli"
	installTarget = modulePath + "/cmd/botbus@latest"
	proxyTimeout  = 2 * time.Second
)

// proxyClient is overridable for tests. nil means use http.DefaultClient
// against proxy.golang.org with the canonical URL.
var proxyClient *http.Client
var proxyURL = "https://proxy.golang.org/" + modulePath + "/@latest"

// currentVersion returns the embedded module pseudo-version of this binary,
// or "" for unstamped / devel builds where no useful comparison is possible.
func currentVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		return ""
	}
	return v
}

// parseProxyLatest extracts {"Version": "...", "Time": "..."} from the
// proxy's @latest response. Returns ("", err) on malformed input.
func parseProxyLatest(body []byte) (string, error) {
	var p struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return "", err
	}
	return p.Version, nil
}

// latestVersion fetches the proxy's @latest record. Bounded by ctx.
func latestVersion(ctx context.Context) (string, error) {
	client := proxyClient
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("proxy status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	return parseProxyLatest(body)
}

// pseudoVersionDate extracts the date portion of a Go pseudo-version like
// "v0.0.0-20260523120000-abcdef123456" → "2026-05-23 12:00 UTC". Returns
// the original string unchanged if it doesn't match the expected shape.
// Used purely for readable display in the update prompt; the comparison
// itself is exact-string-match.
func pseudoVersionDate(v string) string {
	parts := strings.Split(v, "-")
	if len(parts) < 3 {
		return v
	}
	ts := parts[1]
	if len(ts) != 14 {
		return v
	}
	t, err := time.Parse("20060102150405", ts)
	if err != nil {
		return v
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// checkUpdateInteractive runs the full update check + prompt + install flow.
// Skipped if BOTBUS_NO_UPDATE_CHECK is set, if this is a devel build, or if
// the proxy lookup fails. Exits 0 after a successful install so the user
// re-runs with the new binary.
func checkUpdateInteractive() {
	if os.Getenv("BOTBUS_NO_UPDATE_CHECK") != "" {
		return
	}
	cur := currentVersion()
	if cur == "" {
		return // devel build — nothing meaningful to compare
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTimeout)
	defer cancel()
	latest, err := latestVersion(ctx)
	if err != nil || latest == "" || latest == cur {
		return
	}
	fmt.Fprintf(os.Stderr, "Update available: %s → %s.\nInstall now? [Y/n] ",
		pseudoVersionDate(cur), pseudoVersionDate(latest))
	var ans string
	_, _ = fmt.Scanln(&ans) // empty input = accept default (yes)
	ans = strings.ToLower(strings.TrimSpace(ans))
	if ans == "n" || ans == "no" {
		return
	}
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Fprintln(os.Stderr, "`go` not found on PATH — install Go and re-run, or:")
		fmt.Fprintln(os.Stderr, "  go install "+installTarget)
		return
	}
	cmd := exec.Command("go", "install", installTarget)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		return
	}
	fmt.Fprintln(os.Stderr, "Updated. Re-run `botbus` to use the new version.")
	os.Exit(0)
}
