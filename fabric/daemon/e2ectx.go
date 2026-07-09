package daemon

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

type e2eCtx struct {
	key       [32]byte
	epoch     uint32
	channelID string
	deviceID  string
	devPriv   ed25519.PrivateKey
	ws        *agentstate.Workspace
}

func keyArray(b []byte) ([32]byte, error) {
	var k [32]byte
	if len(b) != 32 {
		return k, fmt.Errorf("e2e: workspace key must be 32 bytes, got %d", len(b))
	}
	copy(k[:], b)
	return k, nil
}

func (d *Daemon) e2eContextFor(agentID string) (*e2eCtx, bool, error) {
	// Hold d.mu for all reads of d.state (Agents + Workspaces) to avoid a data
	// race with attach/detach (Upsert/Remove) and applyRekey, which all write
	// d.state under the lock. Copy all needed values out before releasing.
	d.mu.Lock()
	ws, ok := d.state.WorkspaceFor(agentID)
	if !ok || !ws.E2E {
		d.mu.Unlock()
		return nil, false, nil // plaintext path
	}
	keyBytes := append([]byte(nil), ws.Key...)
	epoch := ws.Epoch
	rootID := ws.RootID
	wsPtr := ws
	ag, agOK := d.state.AgentByID(agentID)
	var signSeed []byte
	if agOK && len(ag.SignSeed) == ed25519.SeedSize {
		signSeed = append([]byte(nil), ag.SignSeed...)
	}
	d.mu.Unlock()

	key, err := keyArray(keyBytes)
	if err != nil {
		return nil, true, err
	}
	if !agOK || len(signSeed) != ed25519.SeedSize {
		return nil, true, errors.New("e2e: agent missing device signing seed")
	}
	return &e2eCtx{
		key:       key,
		epoch:     epoch,
		channelID: rootID,
		deviceID:  agentID,
		devPriv:   ed25519.NewKeyFromSeed(signSeed),
		ws:        wsPtr,
	}, true, nil
}

// currentKey returns the workspace's current symmetric key, read under d.mu so
// the opener observes rotations applied by the roster-ingest loop.
func (d *Daemon) currentKey(ws *agentstate.Workspace) ([32]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(ws.Key) != 32 {
		return [32]byte{}, false
	}
	var k [32]byte
	copy(k[:], ws.Key)
	return k, true
}

// nextCounterSeed returns the value a brand-new (deviceID, channelID, epoch)
// counter should be initialized to, in nanoseconds since the Unix epoch. It is
// a var so tests can substitute a deterministic value.
//
// KNOWN RESIDUAL RISK (accepted tradeoff, not fully closed): this seed is only
// virtually certain, not guaranteed, to exceed whatever high-water mark a peer
// already recorded for this triple before the restart. It relies on
// time.Now() at restart being greater than it was before the restart, which
// wall-clock time does not guarantee across a process restart — an NTP step
// correction, VM suspend/resume, or a container/pod rescheduled onto a
// different host with clock skew can all make the new seed *less than* the
// prior high-water mark. If that happens, the exact bug this mechanism exists
// to fix reappears: a silent, durable, one-directional message drop, except
// now gated by clock skew instead of deterministic. This failure mode is
// hardest to hit on a single stable host and most likely in multi-host /
// orchestrated deployments (containers or VMs that can restart on a different
// machine than the one they last ran on) — precisely the deployments a fleet
// daemon like this targets.
//
// Mitigations currently in place for this residual risk:
//   - openerFor's replay-rejection path (fabric/daemon/inbox.go) logs every
//     rejected counter, so a recurrence is observable instead of silent, even
//     though it cannot distinguish "clock skew after restart" from a genuine
//     duplicate/replay.
//   - TestNextCounterSeedBelowHighWaterMarkIsRejected (e2ectx_test.go)
//     exercises the adversarial low-seed case with a stubbed seed and asserts
//     the (known, accepted) rejection, so the residual risk is explicit and
//     covered rather than implicit.
//
// The sturdier fix — persisting/reloading the last-issued counter (or the
// receiver's high-water mark) to disk instead of reconstructing it from wall-
// clock time — removes this residual risk entirely and is the recommended
// follow-up (see REVIEW_FINDINGS.md's improvement-suggestion #1); it was not
// done here because it requires a durable per-(device,channel,epoch) counter
// store and a decision on write frequency (e.g. every message vs. batched)
// that's a larger change than this fix.
var nextCounterSeed = func() uint64 { return uint64(time.Now().UnixNano()) }

// nextCounter returns the next monotonically-increasing sender counter for the
// (deviceID, channelID, epoch) triple. Counters are held in an in-memory map
// on the Daemon that starts empty on every process start, so a naive
// "start at 1" scheme durably collides with the receiver's replay window
// (fabric/daemon/replay.go), which never decreases and is keyed by the same
// triple: every message from a just-restarted sender would be silently
// dropped until the counter climbed back past whatever high-water mark peers
// already recorded, or the workspace key rotated. To avoid that, the first
// counter ever issued for a given triple in this process is seeded from the
// current wall-clock time (nanoseconds since the Unix epoch) instead of 0, so
// it is virtually certain (see nextCounterSeed's doc comment for the residual
// failure mode this does NOT close) to exceed any counter a peer already saw
// before the restart; every counter issued after that for the same triple
// increments normally by 1 and remains strictly monotonic for the life of the
// process.
//
// Caller must NOT hold d.mu.
func (c *e2eCtx) nextCounter(d *Daemon) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.counters == nil {
		d.counters = make(map[string]uint64)
	}
	k := fmt.Sprintf("%s|%s|%d", c.deviceID, c.channelID, c.epoch)
	if _, seeded := d.counters[k]; !seeded {
		d.counters[k] = nextCounterSeed()
	} else {
		d.counters[k]++
	}
	return d.counters[k], nil
}
