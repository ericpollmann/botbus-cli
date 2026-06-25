package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
)

func TestRunPresenceRegistersThenHeartbeats(t *testing.T) {
	var registers, heartbeats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			registers.Add(1)
			w.WriteHeader(http.StatusOK)
		case http.MethodPost:
			heartbeats.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	setHeartbeatEvery(20 * time.Millisecond)
	defer setHeartbeatEvery(30 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	a := agentstate.Agent{ID: "myth-compiler", Key: "key-1", InboxChannel: "i", Focus: "compile"}
	runPresence(ctx, control.NewClient(srv.URL), a)

	if registers.Load() != 1 {
		t.Fatalf("registers = %d, want 1", registers.Load())
	}
	if heartbeats.Load() < 2 {
		t.Fatalf("heartbeats = %d, want >=2", heartbeats.Load())
	}
}
