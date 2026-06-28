package daemon

import (
	"crypto/ed25519"
	"errors"
	"fmt"

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
	ws, ok := d.state.WorkspaceFor(agentID)
	if !ok || !ws.E2E {
		return nil, false, nil // plaintext path
	}
	// Read ws.Key and ws.Epoch under d.mu to avoid a data race with applyRekey,
	// which writes them under the lock. Copy the bytes out before releasing.
	d.mu.Lock()
	keyBytes := append([]byte(nil), ws.Key...)
	epoch := ws.Epoch
	d.mu.Unlock()
	key, err := keyArray(keyBytes)
	if err != nil {
		return nil, true, err
	}
	ag, ok := d.state.AgentByID(agentID)
	if !ok || len(ag.SignSeed) != ed25519.SeedSize {
		return nil, true, errors.New("e2e: agent missing device signing seed")
	}
	return &e2eCtx{
		key:       key,
		epoch:     epoch,
		channelID: ws.RootID,
		deviceID:  agentID,
		devPriv:   ed25519.NewKeyFromSeed(ag.SignSeed),
		ws:        ws,
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

// nextCounter returns the next monotonically-increasing sender counter for the
// (deviceID, channelID, epoch) triple, starting at 1. Counters are held in an
// in-memory map on the Daemon; persistence across restarts is out of scope for
// Phase 1 (documented limitation — replay window on the receiver side handles
// restarts by being deliberately conservative).
//
// Caller must NOT hold d.mu.
func (c *e2eCtx) nextCounter(d *Daemon) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.counters == nil {
		d.counters = make(map[string]uint64)
	}
	k := fmt.Sprintf("%s|%s|%d", c.deviceID, c.channelID, c.epoch)
	d.counters[k]++
	return d.counters[k], nil
}
