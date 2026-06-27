package daemon

import (
	"context"
	"log"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

// runRoster subscribes to ws.Roster and ingests every frame (certs, anchor-set
// updates, rekey grants) until ctx is cancelled, resuming from the latest cursor
// after a disconnect.
func runRoster(ctx context.Context, d *Daemon, ws *agentstate.Workspace) {
	cursor := ""
	for {
		if ctx.Err() != nil {
			return
		}
		frames, err := d.hub.Subscribe(ctx, ws.Roster, cursor)
		if err != nil {
			log.Printf("daemon: subscribe roster %s: %v", ws.Roster, err)
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
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
				d.ingestRosterFrame(ws, fr.Body)
				if fr.Resume != "" {
					cursor = fr.Resume
				}
			}
		}
		if !sleepCtx(ctx, reconnectBackoff) {
			return
		}
	}
}
