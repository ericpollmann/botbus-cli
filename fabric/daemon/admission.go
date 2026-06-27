package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"

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
