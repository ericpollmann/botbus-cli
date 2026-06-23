package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
