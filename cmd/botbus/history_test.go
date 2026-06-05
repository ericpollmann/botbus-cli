package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestFetchHistoryParsesPage(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"msgs":[{"id":"2-0","d":"` + b64("bob: hi") + `"},{"id":"1-0","d":"` + b64("alice: yo") + `"}],"next":"1-0"}`))
	}))
	defer srv.Close()

	p, err := fetchHistory(context.Background(), srv.URL, "/history", "9-0", 40)
	if err != nil {
		t.Fatalf("fetchHistory: %v", err)
	}
	if gotPath != "/history" {
		t.Errorf("path = %q, want /history", gotPath)
	}
	q, _ := url.ParseQuery(gotQuery)
	if q.Get("before") != "9-0" || q.Get("n") != "40" {
		t.Errorf("query = %q, want before=9-0&n=40", gotQuery)
	}
	if len(p.Msgs) != 2 || p.Msgs[0].ID != "2-0" || p.Next != "1-0" {
		t.Errorf("parsed page wrong: %+v", p)
	}
}

func TestFetchHistoryOmitsBeforeWhenEmpty(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"msgs":[],"next":""}`))
	}))
	defer srv.Close()
	if _, err := fetchHistory(context.Background(), srv.URL, "/history", "", 40); err != nil {
		t.Fatalf("fetchHistory: %v", err)
	}
	q, _ := url.ParseQuery(gotQuery)
	if q.Has("before") {
		t.Errorf("before should be omitted when empty, got query %q", gotQuery)
	}
}

func TestFetchHistoryErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := fetchHistory(context.Background(), srv.URL, "/history", "", 40); err == nil {
		t.Error("want error on non-200")
	}
}

func TestDecodeHistFrame(t *testing.T) {
	if got := decodeHistFrame(b64("hello")); string(got) != "hello" {
		t.Errorf("decode = %q, want hello", got)
	}
	if got := decodeHistFrame("!!!not-base64!!!"); got != nil {
		t.Errorf("malformed base64 should decode to nil, got %v", got)
	}
}

func TestHistFramesChronoReverses(t *testing.T) {
	// Page is newest-first; histFramesChrono must return oldest-first.
	p := histPage{Msgs: []histMsg{
		{ID: "3-0", D: b64("c")},
		{ID: "2-0", D: b64("b")},
		{ID: "1-0", D: b64("a")},
	}}
	frames := histFramesChrono(p)
	got := string(frames[0]) + string(frames[1]) + string(frames[2])
	if got != "abc" {
		t.Errorf("chrono order = %q, want abc", got)
	}
}

func TestHistFramesChronoSkipsBadFrames(t *testing.T) {
	p := histPage{Msgs: []histMsg{{ID: "2-0", D: b64("ok")}, {ID: "1-0", D: "@@bad@@"}}}
	frames := histFramesChrono(p)
	if len(frames) != 1 || string(frames[0]) != "ok" {
		t.Errorf("want only the decodable frame, got %v", frames)
	}
}
