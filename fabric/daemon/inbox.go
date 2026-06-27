package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/e2e"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// opener is a per-agent function that processes a single inbound envelope.
// For e2e agents it decrypts, verifies signature, and checks the replay window.
// For non-e2e agents it passes cleartext envelopes through and drops encrypted ones.
// Returns (processed envelope, true) to keep the frame or ({}, false) to drop it.
type opener func(e envelope.Envelope) (envelope.Envelope, bool)

// openerFor builds the opener function for the given receiving agent.
// It captures the agent's e2e context once (no lock held in the closure itself);
// the closure calls d.devices.lookup and d.replay.accept, both of which are
// internally locked.
func (d *Daemon) openerFor(agentID string) opener {
	ec, isE2E, err := d.e2eContextFor(agentID)
	if !isE2E || err != nil {
		// Non-e2e agent: pass cleartext, drop encrypted frames (fail-closed).
		return func(e envelope.Envelope) (envelope.Envelope, bool) {
			if e.Enc != "" {
				return envelope.Envelope{}, false
			}
			return e, true
		}
	}
	return func(e envelope.Envelope) (envelope.Envelope, bool) {
		if e.Enc == "" {
			// E2E agents are fail-closed: drop all unencrypted inbound frames.
			// The connect welcome is delivered locally (never traverses the relay).
			return envelope.Envelope{}, false
		}
		raw, derr := base64.StdEncoding.DecodeString(e.Enc)
		if derr != nil {
			return envelope.Envelope{}, false
		}
		env, perr := e2e.Parse(raw)
		if perr != nil {
			return envelope.Envelope{}, false
		}
		dev, counter, content, oerr := e2e.OpenMessage(ec.key, ec.channelID, env, d.devices.lookup)
		if oerr != nil {
			return envelope.Envelope{}, false
		}
		if !d.replay.accept(replayKey{device: dev, channel: ec.channelID, epoch: env.KeyEpoch}, counter) {
			return envelope.Envelope{}, false
		}
		subj, body, cerr := decodeContent(content)
		if cerr != nil {
			return envelope.Envelope{}, false
		}
		e.Subject = subj
		e.Body = body
		e.Enc = ""
		return e, true
	}
}

// reconnectBackoff is the pause between a dropped inbox subscription and the
// next re-subscribe. Var so tests can shrink it.
var reconnectBackoff = 2 * time.Second

// runInbox subscribes to one agent's inbox channel and feeds its runtime,
// resuming from `cursor` and re-subscribing from the latest seen cursor after
// any disconnect, until ctx is cancelled. persist is called with each new
// cursor so it can be written to local state.
func runInbox(ctx context.Context, rt *AgentRuntime, hub hubclient.HubClient, inbox, cursor string, persist func(string), open opener) {
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
				for _, inner := range unwrap(fr.Body, open) {
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

// unwrap turns a router delivery frame body into the inner envelopes, applying
// open to each one. A kind:"batch" envelope carries a JSON array in its Body;
// anything else is treated as a single envelope (forward-compat / direct frames).
// Frames for which open returns false are dropped.
func unwrap(body string, open opener) []envelope.Envelope {
	e, err := envelope.Decode([]byte(body))
	if err != nil {
		return nil
	}
	var candidates []envelope.Envelope
	if e.Kind != envelope.KindBatch {
		candidates = []envelope.Envelope{e}
	} else {
		var inner []envelope.Envelope
		if err := json.Unmarshal([]byte(e.Body), &inner); err != nil {
			log.Printf("daemon: bad batch body: %v", err)
			return nil
		}
		candidates = inner
	}
	result := candidates[:0:0]
	for _, c := range candidates {
		if out, ok := open(c); ok {
			result = append(result, out)
		}
	}
	return result
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
