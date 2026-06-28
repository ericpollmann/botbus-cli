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
	Kind       string    `json:"kind"`                 // "cert" | "anchors"
	Cert       *e2e.Cert `json:"cert,omitempty"`       // Kind=="cert"
	AnchorBlob []byte    `json:"anchorBlob,omitempty"` // admin-signed device-set blob, Kind=="anchors"
	AnchorSig  []byte    `json:"anchorSig,omitempty"`  // admin signature over AnchorBlob
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
	// Snapshot mutable workspace fields under d.mu — the state watcher may rewrite
	// ws.Key/ws.Epoch concurrently (it writes them under d.mu).
	d.mu.Lock()
	keyBytes := append([]byte(nil), ws.Key...)
	epoch := ws.Epoch
	roster := ws.Roster
	d.mu.Unlock()
	key, err := keyArray(keyBytes)
	if err != nil {
		return err
	}
	b64, err := sealRosterFrame(key, epoch, rosterFrame{Kind: "cert", Cert: &c})
	if err != nil {
		return err
	}
	return d.hub.Publish(ctx, roster, b64)
}

// ingestRosterFrame processes an inbound roster-channel frame.
// Rekey grants are published cleartext (JSON, starts with '{'); cert/anchors
// frames are base64-sealed envelopes (never start with '{').
//
// Any decryption or parse error is silently ignored (fail-closed: a frame we
// cannot authenticate provides no trust).
func (d *Daemon) ingestRosterFrame(ws *agentstate.Workspace, b64 string) {
	if len(b64) > 0 && b64[0] == '{' {
		d.ingestRekeyGrant(ws, b64)
		return
	}
	// Snapshot mutable workspace fields under d.mu — the state watcher may rewrite
	// ws.Key concurrently (it writes under d.mu).
	d.mu.Lock()
	keyBytes := append([]byte(nil), ws.Key...)
	adminPub := ed25519.PublicKey(append([]byte(nil), ws.AdminPub...))
	d.mu.Unlock()
	key, err := keyArray(keyBytes)
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
		if err := d.trust.applyAnchorSet(f.AnchorBlob, f.AnchorSig, adminPub); err != nil {
			log.Printf("roster: applyAnchorSet failed: %v", err)
		}
	}
}

// ingestRekeyGrant adopts a signed rekey grant addressed to a local anchor.
func (d *Daemon) ingestRekeyGrant(ws *agentstate.Workspace, js string) {
	g, err := parseAdmitGrant([]byte(js))
	if err != nil || len(g.WrappedKey) == 0 {
		return
	}
	// Snapshot the local anchor's enc-priv and the workspace admin pubkey under
	// d.mu — reloadFromDisk/attach may append d.state.Agents concurrently.
	d.mu.Lock()
	ag, ok := d.state.AgentByID(g.AnchorID)
	var encPriv []byte
	if ok {
		encPriv = append([]byte(nil), ag.EncPriv...)
	}
	adminPub := append([]byte(nil), ws.AdminPub...)
	d.mu.Unlock()
	if !ok || len(encPriv) != 32 {
		return // not addressed to one of our local anchors
	}
	key, epoch, ok := ProcessRekey(g, encPriv, adminPub)
	if !ok {
		return
	}
	d.applyRekey(ws, key, epoch)
}

// applyRekey installs a newly-adopted key/epoch under d.mu and persists it.
func (d *Daemon) applyRekey(ws *agentstate.Workspace, key [32]byte, epoch uint32) {
	d.mu.Lock()
	if epoch >= ws.Epoch { // monotonic: never roll backwards
		ws.Key = key[:]
		ws.Epoch = epoch
	}
	d.mu.Unlock()
	d.persistWorkspaceKey(ws)
}
