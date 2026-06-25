package daemon

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
	"github.com/ericpollmann/botbus-proto/wire"
)

// heartbeatEveryNs is the presence-refresh interval in nanoseconds (well under
// the router's 90s lease TTL). Atomic so tests can shrink it race-free.
var heartbeatEveryNs atomic.Int64

func init() { heartbeatEveryNs.Store(int64(30 * time.Second)) }

// getHeartbeatEvery reads the current interval. Use setHeartbeatEvery in tests.
func getHeartbeatEvery() time.Duration { return time.Duration(heartbeatEveryNs.Load()) }

// setHeartbeatEvery sets the interval (test helper).
func setHeartbeatEvery(d time.Duration) { heartbeatEveryNs.Store(int64(d)) }

// runPresence re-registers the agent once (idempotent; replays desired state
// so a router/Redis restart self-heals) then heartbeats on a ticker until ctx
// is cancelled.
func runPresence(ctx context.Context, ctl *control.Client, a agentstate.Agent) {
	if err := ctl.Register(ctx, a.ID, a.Key, specOf(a)); err != nil {
		log.Printf("daemon: register %s: %v", a.ID, err)
	}
	t := time.NewTicker(getHeartbeatEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ctl.Heartbeat(ctx, a.ID, a.Key); err != nil {
				log.Printf("daemon: heartbeat %s: %v", a.ID, err)
			}
		}
	}
}

// specOf maps a local agent entry to the control register body.
func specOf(a agentstate.Agent) wire.AgentSpec {
	return wire.AgentSpec{
		Name: a.Name, InboxChannel: a.InboxChannel, Focus: a.Focus,
		Interest: a.Interest, Parent: a.Parent, Mode: a.Mode,
		BatchMS: a.BatchMS, BatchN: a.BatchN, BatchBytes: a.BatchBytes,
		ModelTier: a.ModelTier,
	}
}
