package daemon

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// reconnectBackoff is the pause between a dropped inbox subscription and the
// next re-subscribe. Var so tests can shrink it.
var reconnectBackoff = 2 * time.Second

// runInbox subscribes to one agent's inbox channel and feeds its runtime,
// resuming from `cursor` and re-subscribing from the latest seen cursor after
// any disconnect, until ctx is cancelled. persist is called with each new
// cursor so it can be written to local state.
func runInbox(ctx context.Context, rt *AgentRuntime, hub hubclient.HubClient, inbox, cursor string, persist func(string)) {
	for {
		if ctx.Err() != nil {
			return
		}
		frames, err := hub.Subscribe(ctx, inbox, cursor)
		if err != nil {
			log.Printf("daemon: subscribe %s: %v", inbox, err)
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
		// Read frames until the stream closes (disconnect) or ctx is cancelled.
		// Selecting on ctx.Done means cancellation is prompt even if the hub
		// client's channel doesn't close on its own.
		disconnected := false
		for !disconnected {
			select {
			case <-ctx.Done():
				return
			case fr, ok := <-frames:
				if !ok {
					disconnected = true
					break
				}
				for _, inner := range unwrap(fr.Body) {
					rt.enqueue(inner)
				}
				if fr.Resume != "" {
					cursor = fr.Resume
					persist(cursor)
				}
			}
		}
		// Disconnected — back off and resume from cursor.
		if !sleepCtx(ctx, reconnectBackoff) {
			return
		}
	}
}

// unwrap turns a router delivery frame body into the inner envelopes. A
// kind:"batch" envelope carries a JSON array in its Body; anything else is
// treated as a single envelope (forward-compat / direct frames).
func unwrap(body string) []envelope.Envelope {
	e, err := envelope.Decode([]byte(body))
	if err != nil {
		return nil
	}
	if e.Kind != envelope.KindBatch {
		return []envelope.Envelope{e}
	}
	var inner []envelope.Envelope
	if err := json.Unmarshal([]byte(e.Body), &inner); err != nil {
		log.Printf("daemon: bad batch body: %v", err)
		return nil
	}
	return inner
}

// sleepCtx sleeps for d or until ctx is done; returns false if ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
