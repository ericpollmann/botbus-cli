package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// fetchBriefing GETs the URL and returns the served body verbatim. Driven by an
// httptest.Server so no live network is touched.
func TestFetchBriefingReturnsBody(t *testing.T) {
	const briefing = "you are alice; your inbox is xyz; here is your role briefing"
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(briefing))
	}))
	defer srv.Close()

	body, err := fetchBriefing(srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatalf("fetchBriefing: %v", err)
	}
	if body != briefing {
		t.Fatalf("body = %q, want %q", body, briefing)
	}
	if gotUA != userAgent() {
		t.Fatalf("User-Agent = %q, want %q", gotUA, userAgent())
	}
}

// A non-200 response surfaces an error (including the status).
func TestFetchBriefingNon200Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := fetchBriefing(srv.Client(), srv.URL+"/"); err == nil {
		t.Fatal("a non-200 response should surface an error")
	}
}

// A network/transport failure surfaces an error rather than an empty body.
func TestFetchBriefingNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/"
	srv.Close() // server is now down → the GET must fail

	if _, err := fetchBriefing(http.DefaultClient, url); err == nil {
		t.Fatal("a network error should surface an error")
	}
}
