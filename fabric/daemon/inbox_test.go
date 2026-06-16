package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// makeBatch wraps inner envelopes the way the router's deliver() does.
func makeBatch(t *testing.T, resume string, inner ...envelope.Envelope) hubclient.Frame {
	t.Helper()
	body, _ := json.Marshal(inner)
	wrap := envelope.Envelope{V: 1, ID: envelope.NewID(), From: "router", Kind: envelope.KindBatch, Body: string(body)}
	raw, _ := envelope.Encode(wrap)
	return hubclient.Frame{Name: "router", Body: string(raw), Resume: resume}
}

func TestRunInboxUnwrapsBatchAndAdvancesCursor(t *testing.T) {
	fake := hubclient.NewFake()
	rt := newRuntime("myth-compiler", 100)

	cursorCh := make(chan string, 4)
	persist := func(cursor string) { cursorCh <- cursor }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runInbox(ctx, rt, fake, "inbox-compiler", "", persist)

	// Let the subscription establish, then inject a router batch frame.
	time.Sleep(30 * time.Millisecond)
	fake.Inject("inbox-compiler", makeBatch(t, "cursor-1",
		envelope.Envelope{ID: "m1", From: "eric", Body: "build"},
		envelope.Envelope{ID: "m2", From: "eric", Body: "test"},
	))

	deadline := time.After(2 * time.Second)
	for {
		if got := rt.drain(); len(got) == 2 {
			if got[0].ID != "m1" || got[1].ID != "m2" {
				t.Fatalf("inner envelopes wrong: %+v", got)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("inbox never delivered the unwrapped envelopes")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	select {
	case got := <-cursorCh:
		if got != "cursor-1" {
			t.Fatalf("cursor = %q, want cursor-1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("cursor never persisted")
	}
}
