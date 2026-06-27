package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// JoinRequest is sent by an anchor agent to the workspace's waiting-room
// channel to request admission.
type JoinRequest struct {
	ReqID        string `json:"reqId"`
	Name         string `json:"name"`
	ParentIntent string `json:"parentIntent,omitempty"`
	SignPub      []byte `json:"signPub"`
	EncPub       []byte `json:"encPub"`
}

// AdmitGrant is sent by the workspace admin back to the requesting anchor
// after verifying the SAS fingerprint and approving admission.
type AdmitGrant struct {
	ReqID       string `json:"reqId"`
	AnchorID    string `json:"anchorId"`
	RootID      string `json:"rootId"`
	Epoch       uint32 `json:"epoch"`
	WrappedKey  []byte `json:"wrappedKey"`
	AdminPub    []byte `json:"adminPub"`
	Roster      string `json:"roster"`
	WaitingRoom string `json:"waitingRoom"`
	Sig         []byte `json:"sig,omitempty"`
}

// grantSignedPayload returns the canonical byte string that the admin signs
// when issuing an AdmitGrant. The Sig field itself is NOT included.
//
// Format: domain-tag + LP(ReqID) + LP(AnchorID) + LP(RootID) +
//
//	Epoch(uint32-LE) + LP(WrappedKey) + LP(AdminPub) +
//	LP(Roster) + LP(WaitingRoom)
//
// where LP(x) = uint32-LE len(x) followed by x.
func grantSignedPayload(g AdmitGrant) []byte {
	lp := func(b []byte) []byte {
		prefix := make([]byte, 4)
		binary.LittleEndian.PutUint32(prefix, uint32(len(b)))
		return append(prefix, b...)
	}
	lpStr := func(s string) []byte { return lp([]byte(s)) }

	var out []byte
	out = append(out, []byte("botbus-e2e-grant-v1\x00")...)
	out = append(out, lpStr(g.ReqID)...)
	out = append(out, lpStr(g.AnchorID)...)
	out = append(out, lpStr(g.RootID)...)
	epoch := make([]byte, 4)
	binary.LittleEndian.PutUint32(epoch, g.Epoch)
	out = append(out, epoch...)
	out = append(out, lp(g.WrappedKey)...)
	out = append(out, lp(g.AdminPub)...)
	out = append(out, lpStr(g.Roster)...)
	out = append(out, lpStr(g.WaitingRoom)...)
	return out
}

// Marshal serialises a JoinRequest to JSON.
func (r JoinRequest) Marshal() ([]byte, error) { return json.Marshal(r) }

func parseJoinRequest(b []byte) (JoinRequest, error) {
	var r JoinRequest
	return r, json.Unmarshal(b, &r)
}

// Marshal serialises an AdmitGrant to JSON.
func (g AdmitGrant) Marshal() ([]byte, error) { return json.Marshal(g) }

func parseAdmitGrant(b []byte) (AdmitGrant, error) {
	var g AdmitGrant
	return g, json.Unmarshal(b, &g)
}

// wrapKey seals the 32-byte workspaceKey to anchorEncPub using an anonymous
// sealed box (NaCl box.SealAnonymous). The recipient decrypts with unwrapKey.
func wrapKey(workspaceKey [32]byte, anchorEncPub [32]byte) ([]byte, error) {
	out, err := box.SealAnonymous(nil, workspaceKey[:], &anchorEncPub, rand.Reader)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// unwrapKey derives the X25519 public key from encPriv, then opens the sealed
// box produced by wrapKey. Returns (key, true) on success, ([32]byte{}, false)
// on any failure (wrong key, truncated blob, etc.).
func unwrapKey(blob []byte, encPriv [32]byte) ([32]byte, bool) {
	encPubBytes, err := curve25519.X25519(encPriv[:], curve25519.Basepoint)
	if err != nil {
		return [32]byte{}, false
	}
	var pub [32]byte
	copy(pub[:], encPubBytes)
	plain, ok := box.OpenAnonymous(nil, blob, &pub, &encPriv)
	if !ok || len(plain) != 32 {
		return [32]byte{}, false
	}
	var key [32]byte
	copy(key[:], plain)
	return key, true
}

// AdmitJoinRequest admits req into the workspace: it expands the anchor set,
// publishes the new admin-signed anchor blob to the roster channel, wraps the
// workspace key for the joiner, and delivers the AdmitGrant to the waiting room.
func (d *Daemon) AdmitJoinRequest(ctx context.Context, ws *agentstate.Workspace, req JoinRequest) (AdmitGrant, error) {
	// 1. Build new anchor set = current anchors + joiner.
	devices := d.trust.anchors.snapshot()
	devices[req.ReqID] = req.SignPub

	blob := marshalDeviceSet(signedDeviceSet{Epoch: ws.Epoch, Devices: devices})
	adminPriv := ed25519.NewKeyFromSeed(ws.AdminPriv)
	sig := ed25519.Sign(adminPriv, blob)

	// Apply locally.
	if err := d.trust.applyAnchorSet(blob, sig, ed25519.PublicKey(ws.AdminPub)); err != nil {
		return AdmitGrant{}, err
	}

	// Record the joiner's enc pubkey for future key rotation re-wraps.
	// In-memory only: not persisted across restarts (see anchorEnc field comment).
	if len(req.EncPub) == 32 {
		d.mu.Lock()
		if d.anchorEnc == nil {
			d.anchorEnc = make(map[string][]byte)
		}
		enc := make([]byte, 32)
		copy(enc, req.EncPub)
		d.anchorEnc[req.ReqID] = enc
		d.mu.Unlock()
	}

	// Publish anchor update to roster.
	key, err := keyArray(ws.Key)
	if err != nil {
		return AdmitGrant{}, err
	}
	sealed, err := sealRosterFrame(key, ws.Epoch, rosterFrame{Kind: "anchors", AnchorBlob: blob, AnchorSig: sig})
	if err != nil {
		return AdmitGrant{}, err
	}
	if err := d.hub.Publish(ctx, ws.Roster, sealed); err != nil {
		return AdmitGrant{}, err
	}

	// 2. Wrap workspace key to the joiner's enc public key.
	var encPub [32]byte
	copy(encPub[:], req.EncPub)
	wrapped, err := wrapKey(key, encPub)
	if err != nil {
		return AdmitGrant{}, err
	}

	// 3. Build, sign, and publish the grant.
	grant := AdmitGrant{
		ReqID:       req.ReqID,
		AnchorID:    req.ReqID,
		RootID:      ws.RootID,
		Epoch:       ws.Epoch,
		WrappedKey:  wrapped,
		AdminPub:    ws.AdminPub,
		Roster:      ws.Roster,
		WaitingRoom: ws.WaitingRoom,
	}
	grant.Sig = ed25519.Sign(ed25519.NewKeyFromSeed(ws.AdminPriv), grantSignedPayload(grant))
	grantBytes, err := grant.Marshal()
	if err != nil {
		return AdmitGrant{}, err
	}
	if err := d.hub.Publish(ctx, ws.WaitingRoom, string(grantBytes)); err != nil {
		return AdmitGrant{}, err
	}
	return grant, nil
}

// ProcessAdmitGrant unwraps the workspace key from the grant and returns a
// populated Workspace. Returns (nil, [32]byte{}, false) on any failure.
//
// expectedAdminPub is the Ed25519 public key of the admin the joiner verified
// out-of-band (e.g. via SAS fingerprint). The grant is rejected if AdminPub
// doesn't match, or if the grant signature is invalid.
func ProcessAdmitGrant(grant AdmitGrant, encPriv []byte, expectedAdminPub []byte) (*agentstate.Workspace, [32]byte, bool) {
	if len(encPriv) != 32 {
		return nil, [32]byte{}, false
	}
	// Reject if expectedAdminPub is absent or doesn't match the grant's AdminPub.
	if len(expectedAdminPub) == 0 || !bytes.Equal(grant.AdminPub, expectedAdminPub) {
		return nil, [32]byte{}, false
	}
	// Verify the admin signed this grant (authenticates the sealed box).
	if !ed25519.Verify(ed25519.PublicKey(grant.AdminPub), grantSignedPayload(grant), grant.Sig) {
		return nil, [32]byte{}, false
	}
	var priv [32]byte
	copy(priv[:], encPriv)
	key, ok := unwrapKey(grant.WrappedKey, priv)
	if !ok {
		return nil, [32]byte{}, false
	}
	ws := &agentstate.Workspace{
		RootID:      grant.RootID,
		E2E:         true,
		Epoch:       grant.Epoch,
		Key:         key[:],
		AdminPub:    grant.AdminPub,
		Roster:      grant.Roster,
		WaitingRoom: grant.WaitingRoom,
	}
	return ws, key, true
}

// sasFingerprint returns a short human-comparable string derived from
// sha256(signPub || encPub). The first 60 bits of the hash are encoded as
// Crockford base32 and formatted as "XXXX-XXXX-XXXX" (14 chars total) for
// out-of-band verification during the admission flow.
func sasFingerprint(signPub, encPub []byte) string {
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	h := sha256.New()
	h.Write(signPub)
	h.Write(encPub)
	sum := h.Sum(nil)

	// Pack the first 8 bytes into a uint64, then extract 12 × 5-bit groups
	// (60 bits) by dropping the low 4 bits.
	b := sum[:8]
	bits := (uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])) >> 4
	chars := make([]byte, 12)
	for i := 11; i >= 0; i-- {
		chars[i] = crockford[bits&0x1F]
		bits >>= 5
	}
	return string(chars[0:4]) + "-" + string(chars[4:8]) + "-" + string(chars[8:12])
}
