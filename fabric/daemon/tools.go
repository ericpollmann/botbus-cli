package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// nowRFC3339 is overridable in tests; defaults to wall clock.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }

// Next long-polls the agent's runtime for up to timeoutSec seconds and returns
// the queued envelopes as a JSON array string (empty array on timeout).
func Next(ctx context.Context, rt *AgentRuntime, timeoutSec int) string {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	out := rt.waitNext(cctx)
	if out == nil {
		out = []envelope.Envelope{}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// SendArgs are the agent-supplied fields for an outbound message.
type SendArgs struct {
	To      []string
	Kind    string
	Scope   string
	Subject string
	Body    string
}

// Send stamps id/ts/from onto an outbound envelope and publishes it to the
// daemon's outbound source channel, where the router picks it up.
func Send(ctx context.Context, hub hubclient.HubClient, outboundChannel, from string, a SendArgs) error {
	kind := a.Kind
	if kind == "" {
		kind = envelope.KindChat
	}
	e := envelope.Envelope{
		V: 1, ID: envelope.NewID(), TS: nowRFC3339(), From: from,
		To: a.To, Kind: kind, Scope: a.Scope, Subject: a.Subject, Body: a.Body,
	}
	raw, err := envelope.Encode(e)
	if err != nil {
		return err
	}
	return hub.Publish(ctx, outboundChannel, from+": "+string(raw))
}
