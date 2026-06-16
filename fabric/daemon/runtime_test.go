package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
)

func TestRuntimeEnqueueDrainDedup(t *testing.T) {
	rt := newRuntime("myth-compiler", 100)

	rt.enqueue(envelope.Envelope{ID: "a", Body: "1"})
	rt.enqueue(envelope.Envelope{ID: "b", Body: "2"})
	rt.enqueue(envelope.Envelope{ID: "a", Body: "dup"}) // duplicate id dropped

	got := rt.drain()
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("drain = %+v", got)
	}
	if len(rt.drain()) != 0 {
		t.Fatal("second drain should be empty")
	}
}

func TestRuntimeWaitNextBlocksThenReturns(t *testing.T) {
	rt := newRuntime("a", 100)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		time.Sleep(20 * time.Millisecond)
		rt.enqueue(envelope.Envelope{ID: "x", Body: "hi"})
	}()

	got := rt.waitNext(ctx) // blocks until something is enqueued
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("waitNext = %+v", got)
	}
}

func TestRuntimeWaitNextTimeoutReturnsEmpty(t *testing.T) {
	rt := newRuntime("a", 100)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if got := rt.waitNext(ctx); len(got) != 0 {
		t.Fatalf("expected empty on timeout, got %+v", got)
	}
}
