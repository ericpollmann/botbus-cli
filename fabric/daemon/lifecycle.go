package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

// RotateKey generates a fresh 32-byte workspace key, increments the epoch,
// re-publishes the anchor set at the new epoch, and delivers a "rekey" roster
// frame (wrapping the new key) to every admitted anchor that has a recorded
// enc-pubkey.
//
// The old key is intentionally not erased here — callers and openers that hold
// a short retention window for old-epoch messages continue to work uninterrupted.
//
// anchorEnc is in-memory only and not persisted across daemon restarts; anchors
// that were admitted before the current process started will not receive rekey
// frames. Persisting anchorEnc is a planned future enhancement.
func (d *Daemon) RotateKey(ctx context.Context, ws *agentstate.Workspace) ([32]byte, error) {
	// 1. Generate fresh key and bump epoch.
	var newKey [32]byte
	if _, err := rand.Read(newKey[:]); err != nil {
		return [32]byte{}, err
	}
	newEpoch := ws.Epoch + 1

	// 2. Update workspace record.
	ws.Key = newKey[:]
	ws.Epoch = newEpoch

	// 3. Re-publish anchor set at new epoch.
	adminPriv := ed25519.NewKeyFromSeed(ws.AdminPriv)
	blob := marshalDeviceSet(signedDeviceSet{Epoch: newEpoch, Devices: d.trust.anchors.snapshot()})
	sig := ed25519.Sign(adminPriv, blob)
	if err := d.trust.applyAnchorSet(blob, sig, ed25519.PublicKey(ws.AdminPub)); err != nil {
		return [32]byte{}, err
	}
	anchorsSealed, err := sealRosterFrame(newKey, newEpoch, rosterFrame{Kind: "anchors", AnchorBlob: blob, AnchorSig: sig})
	if err != nil {
		return [32]byte{}, err
	}
	if err := d.hub.Publish(ctx, ws.Roster, anchorsSealed); err != nil {
		return [32]byte{}, err
	}

	// 4. Re-wrap the new key to each admitted anchor that has a recorded enc-pub.
	d.mu.Lock()
	encPubs := make(map[string][]byte, len(d.anchorEnc))
	for id, pub := range d.anchorEnc {
		cp := make([]byte, len(pub))
		copy(cp, pub)
		encPubs[id] = cp
	}
	d.mu.Unlock()

	for anchorID, encPubBytes := range encPubs {
		var encPub [32]byte
		copy(encPub[:], encPubBytes)
		wrapped, err := wrapKey(newKey, encPub)
		if err != nil {
			return [32]byte{}, err
		}
		rekeySealed, err := sealRosterFrame(newKey, newEpoch, rosterFrame{
			Kind:       "rekey",
			WrappedKey: wrapped,
			AnchorID:   anchorID,
		})
		if err != nil {
			return [32]byte{}, err
		}
		if err := d.hub.Publish(ctx, ws.Roster, rekeySealed); err != nil {
			return [32]byte{}, err
		}
	}

	return newKey, nil
}

// RemoveAnchor evicts anchorID from the trust graph, drops its enc-pub record
// so it receives no future rekey frames, and then calls RotateKey so the
// removed anchor cannot decrypt the new epoch's traffic.
func (d *Daemon) RemoveAnchor(ctx context.Context, ws *agentstate.Workspace, anchorID string) ([32]byte, error) {
	// 1. Drop anchor from trust graph and enc-pub map.
	d.trust.anchors.remove(anchorID)
	d.mu.Lock()
	delete(d.anchorEnc, anchorID)
	d.mu.Unlock()

	// 2. Rotate (so the evicted anchor never receives the new key).
	return d.RotateKey(ctx, ws)
}
