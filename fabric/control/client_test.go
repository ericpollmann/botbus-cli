package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ericpollmann/botbus-proto/wire"
)

// stubControl mimics the router's control API contract: it authorizes only the
// given key. Lets us exercise the client without importing the private server.
func stubControl(t *testing.T, goodKey string) *httptest.Server {
	t.Helper()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+goodKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/agents/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientRegisterAndHeartbeat(t *testing.T) {
	srv := stubControl(t, "key-ok")
	ctx := context.Background()
	c := NewClient(srv.URL)

	if err := c.Register(ctx, "myth-boss", "key-ok", wire.AgentSpec{InboxChannel: "inbox-boss", Focus: "coordination"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := c.Heartbeat(ctx, "myth-boss", "key-ok"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

func TestClientRejectsBadStatus(t *testing.T) {
	srv := stubControl(t, "key-ok")
	ctx := context.Background()
	c := NewClient(srv.URL)

	if err := c.Register(ctx, "a", "wrong-key", wire.AgentSpec{InboxChannel: "i1"}); err == nil {
		t.Fatal("expected error registering with a key the server rejects")
	}
	if err := c.Heartbeat(ctx, "a", "wrong-key"); err == nil {
		t.Fatal("expected error on heartbeat with a rejected key")
	}
}
