package e2e

import "crypto/ed25519"

// Cert is a parent-signed certificate that binds a child's signing public key
// to a (childID, parentID) identity pair. The parent's Ed25519 private key
// signs a canonical payload so the cert is self-contained and verifiable with
// only the parent's public key.
type Cert struct {
	ChildID      string `json:"child"`
	ParentID     string `json:"parent"`
	ChildSignPub []byte `json:"childPub"`
	Sig          []byte `json:"sig"`
}

// SignCert creates a Cert signed by parentPriv over the locked payload layout.
func SignCert(parentPriv ed25519.PrivateKey, childID, parentID string, childSignPub ed25519.PublicKey) Cert {
	payload := signedCertPayload(childID, parentID, childSignPub)
	return Cert{
		ChildID:      childID,
		ParentID:     parentID,
		ChildSignPub: []byte(childSignPub),
		Sig:          ed25519.Sign(parentPriv, payload),
	}
}

// VerifyCert returns true iff c.Sig is a valid Ed25519 signature by parentPub
// over the cert's canonical payload.
func VerifyCert(c Cert, parentPub ed25519.PublicKey) bool {
	return ed25519.Verify(parentPub, signedCertPayload(c.ChildID, c.ParentID, c.ChildSignPub), c.Sig)
}

// signedCertPayload builds the canonical byte string that is signed/verified.
// Layout: "botbus-e2e-cert-v1\x00" ‖ lp(childID) ‖ lp(parentID) ‖ lp(childSignPub)
// where lp(x) = uint32-LE length ‖ bytes.
func signedCertPayload(childID, parentID string, childSignPub []byte) []byte {
	buf := append([]byte(nil), "botbus-e2e-cert-v1\x00"...)
	buf = putLenPrefixedString(buf, childID)
	buf = putLenPrefixedString(buf, parentID)
	buf = putLenPrefixedBytes(buf, childSignPub)
	return buf
}
