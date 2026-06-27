package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

// RotateKey generates a fresh 32-byte workspace key, increments the epoch,
// delivers a signed AdmitGrant-shaped rekey to every admitted anchor, and then
// re-publishes the anchor set sealed under the new key.
//
// Rekey grants are published cleartext BEFORE the new anchors frame so that a
// sequential ingester can adopt the new key, then open the newKey-sealed anchors
// frame. The WrappedKey inside each grant is NaCl sealed-box ciphertext, so the
// relay still sees only ciphertext key material (relay-blind).
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

	adminPriv := ed25519.NewKeyFromSeed(ws.AdminPriv)

	// 3. Re-wrap the new key to each persisted anchor as a signed rekey grant
	//    (cleartext on the wire; WrappedKey is sealed-box ciphertext). Published
	//    BEFORE the anchors frame so a sequential ingester adopts the new key
	//    first, then can open the newKey-sealed anchors frame.
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
		grant := AdmitGrant{
			ReqID: ar.ID, AnchorID: ar.ID, RootID: ws.RootID, Epoch: newEpoch,
			WrappedKey: wrapped, AdminPub: ws.AdminPub, Roster: ws.Roster, WaitingRoom: ws.WaitingRoom,
		}
		grant.Sig = ed25519.Sign(adminPriv, grantSignedPayload(grant))
		gb, err := grant.Marshal()
		if err != nil {
			return [32]byte{}, err
		}
		if err := d.hub.Publish(ctx, ws.Roster, string(gb)); err != nil {
			return [32]byte{}, err
		}
	}

	// 4. Re-publish the anchor set at the new epoch (sealed under newKey).
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
