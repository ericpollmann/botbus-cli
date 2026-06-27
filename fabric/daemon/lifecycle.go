package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

// RotateKey generates a fresh 32-byte workspace key, increments the epoch,
// re-publishes the anchor set at the new epoch, and delivers a "rekey" roster
// frame (wrapping the new key) to every admitted anchor recorded in ws.Anchors.
//
// The old key is intentionally not erased here — callers and openers that hold
// a short retention window for old-epoch messages continue to work uninterrupted.
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

	// 4. Re-wrap the new key to each persisted anchor.
	for _, ar := range ws.Anchors {
		if len(ar.EncPub) != 32 {
			continue
		}
		var encPub [32]byte
		copy(encPub[:], ar.EncPub)
		wrapped, err := wrapKey(newKey, encPub)
		if err != nil {
			return [32]byte{}, err
		}
		// (rekey publish — Task 2 changes the wire format; keep current sealed form for now)
		rekeySealed, err := sealRosterFrame(newKey, newEpoch, rosterFrame{Kind: "rekey", WrappedKey: wrapped, AnchorID: ar.ID})
		if err != nil {
			return [32]byte{}, err
		}
		if err := d.hub.Publish(ctx, ws.Roster, rekeySealed); err != nil {
			return [32]byte{}, err
		}
	}

	return newKey, nil
}

// RemoveAnchor evicts anchorID from the trust graph, drops it from ws.Anchors,
// and then calls RotateKey so the removed anchor cannot decrypt the new epoch's traffic.
func (d *Daemon) RemoveAnchor(ctx context.Context, ws *agentstate.Workspace, anchorID string) ([32]byte, error) {
	d.trust.anchors.remove(anchorID)
	out := ws.Anchors[:0]
	for _, ar := range ws.Anchors {
		if ar.ID != anchorID {
			out = append(out, ar)
		}
	}
	ws.Anchors = out
	return d.RotateKey(ctx, ws)
}
