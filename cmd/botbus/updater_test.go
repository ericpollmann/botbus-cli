package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseProxyLatest(t *testing.T) {
	cases := []struct {
		desc    string
		body    string
		want    string
		wantErr bool
	}{
		{"happy", `{"Version":"v0.0.0-20260524000151-f1f40ceb4aa0","Time":"2026-05-24T00:01:51Z"}`,
			"v0.0.0-20260524000151-f1f40ceb4aa0", false},
		{"extra fields ignored", `{"Version":"v1.2.3","Time":"...","Origin":{"VCS":"git"}}`,
			"v1.2.3", false},
		{"no version", `{"Time":"..."}`, "", false},
		{"malformed", `{not-json`, "", true},
		{"empty body", ``, "", true},
	}
	for _, c := range cases {
		got, err := parseProxyLatest([]byte(c.body))
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.desc, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.desc, got, c.want)
		}
	}
}

func TestPseudoVersionDate(t *testing.T) {
	cases := map[string]string{
		"v0.0.0-20260523120000-abcdef123456": "2026-05-23 12:00 UTC",
		"v0.0.0-20240101000000-000000000000": "2024-01-01 00:00 UTC",
		"v1.2.3":                             "v1.2.3", // no date portion
		"":                                   "",
		"v0.0.0-not-a-date":                  "v0.0.0-not-a-date",
		"v0.0.0-2026-abc":                    "v0.0.0-2026-abc", // ts wrong length
	}
	for in, want := range cases {
		if got := pseudoVersionDate(in); got != want {
			t.Errorf("pseudoVersionDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsStrictlyNewer(t *testing.T) {
	cases := []struct {
		desc       string
		latest     string
		cur        string
		wantNewer  bool
	}{
		{"latest newer than cur",
			"v0.0.0-20260524003103-aaa", "v0.0.0-20260524001745-bbb", true},
		{"latest older than cur (proxy lag — the bug scenario)",
			"v0.0.0-20260524001745-bbb", "v0.0.0-20260524003103-aaa", false},
		{"identical timestamps (no re-install loop)",
			"v0.0.0-20260524003103-aaa", "v0.0.0-20260524003103-aaa", false},
		{"identical date, different hash, same time — no offer",
			"v0.0.0-20260524003103-aaa", "v0.0.0-20260524003103-bbb", false},
		{"unparseable latest — conservative no-prompt",
			"v1.2.3", "v0.0.0-20260524003103-aaa", false},
		{"unparseable cur — conservative no-prompt",
			"v0.0.0-20260524003103-aaa", "v1.2.3", false},
		{"both unparseable — no prompt",
			"v1.2.3", "v1.2.4", false},
	}
	for _, c := range cases {
		if got := isStrictlyNewer(c.latest, c.cur); got != c.wantNewer {
			t.Errorf("%s: isStrictlyNewer(%q, %q) = %v, want %v",
				c.desc, c.latest, c.cur, got, c.wantNewer)
		}
	}
}

func TestLatestVersion(t *testing.T) {
	// Mock the proxy with httptest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v0.0.0-20260601000000-aaaaaa","Time":"2026-06-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	prevURL, prevClient := proxyURL, proxyClient
	defer func() { proxyURL, proxyClient = prevURL, prevClient }()
	proxyURL = srv.URL
	proxyClient = srv.Client()

	got, err := latestVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "v0.0.0-20260601000000-aaaaaa"
	if got != want {
		t.Errorf("latestVersion = %q, want %q", got, want)
	}
}

func TestLatestVersionNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	prevURL, prevClient := proxyURL, proxyClient
	defer func() { proxyURL, proxyClient = prevURL, prevClient }()
	proxyURL = srv.URL
	proxyClient = srv.Client()

	if _, err := latestVersion(context.Background()); err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestCurrentVersionInTest(t *testing.T) {
	// Inside `go test`, the binary is unstamped — debug.ReadBuildInfo()
	// reports "" or "(devel)" for Main.Version. We just verify the
	// function doesn't panic and returns a string (potentially empty).
	_ = currentVersion()
}
