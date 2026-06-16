// Package daemon is the host-side botbus daemon: it multiplexes all of a
// host's fabric agents over one process — one inbox subscription, delivery
// queue, and MCP endpoint per agent — so sessions never hold an internet-
// facing SSE connection and big models wake only for their own inbox.
package daemon

import (
	"context"
	"sync"

	"github.com/ericpollmann/botbus-proto/envelope"
)

// AgentRuntime is the live per-agent state inside the daemon: a delivery queue
// of inbound envelopes, an at-least-once dedup set keyed by envelope id, and a
// signal channel for long-pollers.
type AgentRuntime struct {
	ID string

	mu     sync.Mutex
	queue  []envelope.Envelope
	seen   map[string]struct{}
	seenN  int
	maxN   int
	signal chan struct{} // buffered(1); poked on enqueue
}

func newRuntime(id string, maxSeen int) *AgentRuntime {
	return &AgentRuntime{
		ID:     id,
		seen:   make(map[string]struct{}),
		maxN:   maxSeen,
		signal: make(chan struct{}, 1),
	}
}

// enqueue appends an envelope unless its id was already seen, then pokes the
// signal channel (non-blocking) so a waiting waitNext wakes.
func (r *AgentRuntime) enqueue(e envelope.Envelope) {
	r.mu.Lock()
	if _, dup := r.seen[e.ID]; dup {
		r.mu.Unlock()
		return
	}
	r.markSeen(e.ID)
	r.queue = append(r.queue, e)
	r.mu.Unlock()
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// markSeen records an id, bounding the set by clearing it when it grows past
// maxN (dedup is a recent-window guarantee, not forever). Caller holds mu.
func (r *AgentRuntime) markSeen(id string) {
	if r.seenN >= r.maxN {
		r.seen = make(map[string]struct{})
		r.seenN = 0
	}
	r.seen[id] = struct{}{}
	r.seenN++
}

// drain removes and returns all queued envelopes.
func (r *AgentRuntime) drain() []envelope.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.queue
	r.queue = nil
	return out
}

// waitNext returns queued envelopes immediately if any, else blocks until an
// enqueue or ctx cancellation, then returns whatever is queued (possibly empty
// on timeout).
func (r *AgentRuntime) waitNext(ctx context.Context) []envelope.Envelope {
	if got := r.drain(); len(got) > 0 {
		return got
	}
	select {
	case <-ctx.Done():
		return r.drain()
	case <-r.signal:
		return r.drain()
	}
}
