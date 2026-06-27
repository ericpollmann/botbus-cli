package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/e2e"
)

// rosterAAD is the fixed additional-authenticated data for roster frame seals.
// It provides domain separation and confidentiality for certs in transit.
var rosterAAD = []byte("botbus-e2e-roster-v1")

// rosterFrame is the cleartext-before-sealing roster payload.
type rosterFrame struct {
	Kind       string    `json:"kind"`                 // "cert" | "anchors" | "rekey"
	Cert       *e2e.Cert `json:"cert,omitempty"`       // Kind=="cert"
	AnchorBlob []byte    `json:"anchorBlob,omitempty"` // admin-signed device-set blob, Kind=="anchors"
	AnchorSig  []byte    `json:"anchorSig,omitempty"`  // admin signature over AnchorBlob
	WrappedKey []byte    `json:"wrappedKey,omitempty"` // NaCl-sealed new key, Kind=="rekey"
	AnchorID   string    `json:"anchorId,omitempty"`   // target anchor for this rekey frame
}

// sealRosterFrame JSON-encodes f, seals it with e2e.Seal under key/epoch/rosterAAD,
// and returns the base64-encoded marshalled envelope.
func sealRosterFrame(key [32]byte, epoch uint32, f rosterFrame) (string, error) {
	plain, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	env, err := e2e.Seal(key, epoch, rosterAAD, plain)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(env.Marshal()), nil
}

// openRosterFrame decodes base64 b64, parses the envelope, opens it with
// e2e.Open under key/rosterAAD, and JSON-decodes the result into a rosterFrame.
func openRosterFrame(key [32]byte, b64 string) (rosterFrame, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return rosterFrame{}, err
	}
	env, err := e2e.Parse(raw)
	if err != nil {
		return rosterFrame{}, err
	}
	plain, err := e2e.Open(key, rosterAAD, env)
	if err != nil {
		return rosterFrame{}, err
	}
	var f rosterFrame
	if err := json.Unmarshal(plain, &f); err != nil {
		return rosterFrame{}, err
	}
	return f, nil
}

// publishCert seals a cert frame under the workspace key and publishes it to
// the workspace's roster channel.
func (d *Daemon) publishCert(ctx context.Context, ws *agentstate.Workspace, c e2e.Cert) error {
	key, err := keyArray(ws.Key)
	if err != nil {
		return err
	}
	b64, err := sealRosterFrame(key, ws.Epoch, rosterFrame{Kind: "cert", Cert: &c})
	if err != nil {
		return err
	}
	return d.hub.Publish(ctx, ws.Roster, b64)
}

// ingestRosterFrame decrypts a base64 roster frame and applies its content:
//   - Kind=="cert"    → d.trust.addCert
//   - Kind=="anchors" → d.trust.applyAnchorSet (requires valid admin signature)
//
// Any decryption or parse error is silently ignored (fail-closed: a frame we
// cannot authenticate provides no trust).
func (d *Daemon) ingestRosterFrame(ws *agentstate.Workspace, b64 string) {
	key, err := keyArray(ws.Key)
	if err != nil {
		log.Printf("roster: bad workspace key: %v", err)
		return
	}
	f, err := openRosterFrame(key, b64)
	if err != nil {
		// Decryption failed — ignore silently (fail-closed).
		return
	}
	switch f.Kind {
	case "cert":
		if f.Cert == nil {
			return
		}
		d.trust.addCert(*f.Cert)
	case "anchors":
		if len(f.AnchorBlob) == 0 {
			return
		}
		adminPub := ed25519.PublicKey(ws.AdminPub)
		if err := d.trust.applyAnchorSet(f.AnchorBlob, f.AnchorSig, adminPub); err != nil {
			log.Printf("roster: applyAnchorSet failed: %v", err)
		}
	}
}
