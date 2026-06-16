package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

func TestNextReturnsQueued(t *testing.T) {
	rt := newRuntime("a", 100)
	rt.enqueue(envelope.Envelope{ID: "m1", Body: "hello"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	out := Next(ctx, rt, 1)
	var got []envelope.Envelope
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("next output not a JSON envelope array: %v (%s)", err, out)
	}
	if len(got) != 1 || got[0].Body != "hello" {
		t.Fatalf("next = %s", out)
	}
}

func TestNextTimeoutReturnsEmptyArray(t *testing.T) {
	rt := newRuntime("a", 100)
	ctx := context.Background()
	out := Next(ctx, rt, 1) // nothing queued; 1s timeout
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("expected empty array on timeout, got %q", out)
	}
}

func TestSendStampsAndPublishes(t *testing.T) {
	fake := hubclient.NewFake()
	ctx := context.Background()

	err := Send(ctx, fake, "outbound-chan", "myth-compiler", SendArgs{
		To: []string{"myth-boss"}, Kind: "dm", Body: "need review",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	pubs := fake.Published("outbound-chan")
	if len(pubs) != 1 {
		t.Fatalf("want 1 publish, got %d", len(pubs))
	}
	raw := pubs[0]
	prefix := "myth-compiler: "
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("published body missing name prefix: %q", raw)
	}
	e, err := envelope.Decode([]byte(strings.TrimPrefix(raw, prefix)))
	if err != nil {
		t.Fatalf("decode published: %v (%s)", err, raw)
	}
	if e.From != "myth-compiler" || e.Kind != "dm" || e.Body != "need review" || e.ID == "" || e.TS == "" {
		t.Fatalf("bad stamped envelope: %+v", e)
	}
	if len(e.To) != 1 || e.To[0] != "myth-boss" {
		t.Fatalf("bad To: %+v", e.To)
	}
}

func TestSendDefaultsKindToChat(t *testing.T) {
	fake := hubclient.NewFake()
	if err := Send(context.Background(), fake, "out", "a", SendArgs{Body: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw := fake.Published("out")[0]
	e, _ := envelope.Decode([]byte(strings.TrimPrefix(raw, "a: ")))
	if e.Kind != envelope.KindChat {
		t.Fatalf("kind = %q, want chat", e.Kind)
	}
}
