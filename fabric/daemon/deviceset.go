package daemon

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

type deviceSet struct {
	mu    sync.RWMutex
	epoch uint32
	pubs  map[string]ed25519.PublicKey
}

func newDeviceSet() *deviceSet {
	return &deviceSet{pubs: make(map[string]ed25519.PublicKey)}
}

func (d *deviceSet) lookup(deviceID string) (ed25519.PublicKey, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.pubs[deviceID]
	return p, ok
}

func (d *deviceSet) set(deviceID string, pub ed25519.PublicKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pubs[deviceID] = pub
}

type signedDeviceSet struct {
	Epoch   uint32            `json:"epoch"`
	Devices map[string][]byte `json:"devices"`
}

// marshalDeviceSet produces canonical JSON (sorted device ids) so signer and
// verifier hash identical bytes regardless of Go map iteration order.
func marshalDeviceSet(s signedDeviceSet) []byte {
	ids := make([]string, 0, len(s.Devices))
	for id := range s.Devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ordered := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		pair, _ := json.Marshal([]any{id, s.Devices[id]})
		ordered = append(ordered, pair)
	}
	out, _ := json.Marshal(struct {
		Epoch   uint32            `json:"epoch"`
		Devices []json.RawMessage `json:"devices"`
	}{Epoch: s.Epoch, Devices: ordered})
	return out
}

func (d *deviceSet) applySigned(blob, sig []byte, adminPub ed25519.PublicKey) error {
	if !ed25519.Verify(adminPub, blob, sig) {
		return errors.New("e2e: device set signature invalid")
	}
	parsed, err := parseCanonicalDeviceSet(blob)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.epoch = parsed.Epoch
	d.pubs = make(map[string]ed25519.PublicKey, len(parsed.Devices))
	for id, pub := range parsed.Devices {
		d.pubs[id] = ed25519.PublicKey(pub)
	}
	return nil
}

// snapshot returns a copy of the current id→pubBytes map under RLock.
func (d *deviceSet) snapshot() map[string][]byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string][]byte, len(d.pubs))
	for id, pub := range d.pubs {
		cp := make([]byte, len(pub))
		copy(cp, pub)
		out[id] = cp
	}
	return out
}

// parseCanonicalDeviceSet reverses marshalDeviceSet (devices = [[id, pubBytes], ...]).
func parseCanonicalDeviceSet(blob []byte) (signedDeviceSet, error) {
	var raw struct {
		Epoch   uint32              `json:"epoch"`
		Devices [][]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return signedDeviceSet{}, err
	}
	out := signedDeviceSet{Epoch: raw.Epoch, Devices: map[string][]byte{}}
	for _, pair := range raw.Devices {
		if len(pair) != 2 {
			return signedDeviceSet{}, errors.New("e2e: malformed device pair")
		}
		var id string
		var pub []byte
		if err := json.Unmarshal(pair[0], &id); err != nil {
			return signedDeviceSet{}, err
		}
		if err := json.Unmarshal(pair[1], &pub); err != nil {
			return signedDeviceSet{}, err
		}
		if len(pub) != ed25519.PublicKeySize {
			return signedDeviceSet{}, fmt.Errorf("e2e: device %q has invalid pubkey length %d", id, len(pub))
		}
		if _, exists := out.Devices[id]; exists {
			return signedDeviceSet{}, fmt.Errorf("e2e: duplicate device id %q", id)
		}
		out.Devices[id] = pub
	}
	return out, nil
}
