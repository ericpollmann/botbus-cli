package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsResumeServer stands up an httptest WebSocket server that records the
// ?resume= query of every connection on `resumes`. The first connection
// sends "ok" + one real frame then closes (forcing the client to reconnect);
// later connections send "ok" and stay open until the test cancels. Returns
// the ws:// dial target and the recorded-resume channel.
func wsResumeServer(t *testing.T, firstFrame string) (target string, resumes <-chan string) {
	t.Helper()
	ch := make(chan string, 16)
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		conn := n
		mu.Unlock()
		ch <- r.URL.Query().Get("resume")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		if c.Write(ctx, websocket.MessageBinary, []byte("ok")) != nil {
			return
		}
		if conn == 1 {
			_ = c.Write(ctx, websocket.MessageBinary, []byte(firstFrame))
			time.Sleep(20 * time.Millisecond) // let the client read it before we close
			_ = c.Close(websocket.StatusNormalClosure, "bye")
			return
		}
		<-ctx.Done()
		_ = c.CloseNow()
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/", ch
}

func TestRunWSTextSendsResumeOnReconnect(t *testing.T) {
	old := reconnectBackoff
	reconnectBackoff = 10 * time.Millisecond
	defer func() { reconnectBackoff = old }()

	target, resumes := wsResumeServer(t, "alice: hi")

	ctx, cancel := context.WithCancel(context.Background())
	recv := make(chan []byte, 8)
	send := make(chan []byte)
	states := make(chan connState, 32)
	go drainStates(states)
	go drainBytes(recv)
	workerDone := make(chan struct{})
	go func() { runWSText(ctx, target, "", recv, send, states, make(chan seedMsg, 1)); close(workerDone) }()

	// First dial: fresh ring → no resume token.
	if first := recvString(t, resumes); first != "" {
		t.Errorf("first connect should carry no resume token, got %q", first)
	}
	// Reconnect dial: ring holds the one frame from connection 1.
	second := recvString(t, resumes)
	wantFp := hashFrames([][]byte{[]byte("alice: hi")})
	want := "1." + hex.EncodeToString(wantFp[:])
	if second != want {
		t.Errorf("reconnect resume token = %q, want %q", second, want)
	}

	// Stop the worker BEFORE the deferred reconnectBackoff restore runs, so it
	// isn't reading the var while we write it (data race).
	cancel()
	<-workerDone
}

func TestRunWSAudioSendsResumeOnReconnect(t *testing.T) {
	old := reconnectBackoff
	reconnectBackoff = 10 * time.Millisecond
	defer func() { reconnectBackoff = old }()

	target, resumes := wsResumeServer(t, "voiceframe")

	ctx, cancel := context.WithCancel(context.Background())
	audio := make(chan []byte, 8)
	go drainBytes(audio)
	workerDone := make(chan struct{})
	go func() { runWSAudio(ctx, target, "", audio); close(workerDone) }()

	if first := recvString(t, resumes); first != "" {
		t.Errorf("first audio connect should carry no resume token, got %q", first)
	}
	second := recvString(t, resumes)
	wantFp := hashFrames([][]byte{[]byte("voiceframe")})
	want := "1." + hex.EncodeToString(wantFp[:])
	if second != want {
		t.Errorf("reconnect audio resume token = %q, want %q", second, want)
	}

	// Stop the worker before the deferred reconnectBackoff restore (race-free).
	cancel()
	<-workerDone
}

// recvString reads one value from resumes with a timeout so a wiring bug
// fails fast instead of hanging the suite.
func recvString(t *testing.T, c <-chan string) string {
	t.Helper()
	select {
	case s := <-c:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a connection")
		return ""
	}
}

func drainStates(c <-chan connState) {
	for range c {
	}
}

func drainBytes(c <-chan []byte) {
	for range c {
	}
}
