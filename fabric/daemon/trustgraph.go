package daemon

import (
	"crypto/ed25519"
	"sync"

	"github.com/ericpollmann/botbus-cli/fabric/e2e"
)

// trustGraph decides whether an agent id is trusted by resolving a
// parent-signed cert chain up to an admitted anchor.
type trustGraph struct {
	anchors *deviceSet // admin-signed admitted-anchor set (id -> signPub)
	mu      sync.RWMutex
	certs   map[string]e2e.Cert // childID -> parent-signed cert
}

func newTrustGraph() *trustGraph {
	return &trustGraph{
		anchors: newDeviceSet(),
		certs:   make(map[string]e2e.Cert),
	}
}

// addCert stores the cert indexed by its ChildID.
func (g *trustGraph) addCert(c e2e.Cert) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.certs[c.ChildID] = c
}

// applyAnchorSet delegates to the embedded anchor deviceSet.
func (g *trustGraph) applyAnchorSet(blob, sig []byte, adminPub ed25519.PublicKey) error {
	return g.anchors.applySigned(blob, sig, adminPub)
}

// resolve returns the signing public key for id if it can be traced to an
// admitted anchor through a valid cert chain.
//
// Lock strategy: hold g.mu.RLock for the entire walk so that a concurrent
// addCert cannot mutate g.certs while resolveLocked reads it. anchors.lookup
// takes the deviceSet's own mutex — no lock-ordering hazard since nothing
// acquires g.mu while holding the deviceSet mutex.
func (g *trustGraph) resolve(id string) (ed25519.PublicKey, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.resolveLocked(id, map[string]bool{})
}

// resolveLocked is the recursive helper; caller must hold g.mu.RLock.
// visited guards against cycles.
func (g *trustGraph) resolveLocked(id string, visited map[string]bool) (ed25519.PublicKey, bool) {
	// Cycle / depth guard.
	if visited[id] {
		return nil, false
	}
	visited[id] = true

	// 1. Admitted anchor → done.
	if pub, ok := g.anchors.lookup(id); ok {
		return pub, true
	}

	// 2. Must have a cert.
	c, ok := g.certs[id]
	if !ok {
		return nil, false
	}

	// 3. Recursively resolve the parent.
	parentPub, ok := g.resolveLocked(c.ParentID, visited)
	if !ok {
		return nil, false
	}

	// 4. Verify the cert with the parent's resolved pub.
	if !e2e.VerifyCert(c, parentPub) {
		return nil, false
	}

	return ed25519.PublicKey(c.ChildSignPub), true
}
