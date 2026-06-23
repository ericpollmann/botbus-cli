package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchBoardDecodesColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/board" {
			t.Errorf("expected /board, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"in_progress":[{"task":"t1","title":"Build it","by":"eric"}],"blocked":[],"done":[{"task":"t0","title":"Done thing","by":"bot"}]}`))
	}))
	defer srv.Close()

	b, err := fetchBoard(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchBoard: %v", err)
	}
	if len(b.InProgress) != 1 || b.InProgress[0].Title != "Build it" || b.InProgress[0].By != "eric" {
		t.Fatalf("in_progress not decoded: %+v", b.InProgress)
	}
	if len(b.Done) != 1 || b.Done[0].Title != "Done thing" {
		t.Fatalf("done not decoded: %+v", b.Done)
	}
}

func TestFetchBoardErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fetchBoard(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}
