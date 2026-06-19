// Package console holds the botbus operator-console helpers that aren't part of
// the TUI event loop: welcome rendering/seeding and (future) roster shaping.
package console

import (
	"context"
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/profile"
	"github.com/ericpollmann/botbus-proto/envelope"
	"github.com/ericpollmann/botbus-proto/hubclient"
)

// RenderWelcome produces the first-message text an agent sees on its channel.
func RenderWelcome(agentName, focus, parentName string, p *profile.Profile) string {
	return fmt.Sprintf(
		"Welcome to your private botbus channel. You're %q, working with %s, who %s. "+
			"You report to %q. Focus: %s. This is a chat channel — read with `next`, send with `send`, "+
			"ask for detail any time. You'll get short, debounced updates aimed just at you, not a firehose.",
		agentName, p.User, p.Framing, parentName, focus,
	)
}

// SeedWelcome publishes the welcome as a chat envelope into the agent's inbox so
// it is the first message the agent sees on connect.
func SeedWelcome(ctx context.Context, hub hubclient.HubClient, inbox, text string) error {
	e := envelope.Envelope{V: 1, ID: envelope.NewID(), From: "botbus", Kind: envelope.KindChat, Body: text}
	raw, err := envelope.Encode(e)
	if err != nil {
		return err
	}
	return hub.Publish(ctx, inbox, "botbus: "+string(raw))
}
