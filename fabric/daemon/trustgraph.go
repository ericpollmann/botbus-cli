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
// Lock strategy: take g.mu.RLock once at the top and snapshot the certs map
// into a local reference. anchors.lookup takes the deviceSet's own mutex, so
// there is no lock-ordering issue.
func (g *trustGraph) resolve(id string) (ed25519.PublicKey, bool) {
	g.mu.RLock()
	certs := g.certs // local reference; map entries are immutable once stored
	g.mu.RUnlock()

	visited := make(map[string]bool, len(certs)+1)
	return resolveChain(id, certs, g.anchors, visited)
}

// resolveChain is the recursive helper. visited guards against cycles.
func resolveChain(id string, certs map[string]e2e.Cert, anchors *deviceSet, visited map[string]bool) (ed25519.PublicKey, bool) {
	// Cycle / depth guard.
	if visited[id] {
		return nil, false
	}
	visited[id] = true

	// 1. Admitted anchor → done.
	if pub, ok := anchors.lookup(id); ok {
		return pub, true
	}

	// 2. Must have a cert.
	c, ok := certs[id]
	if !ok {
		return nil, false
	}

	// 3. Recursively resolve the parent.
	parentPub, ok := resolveChain(c.ParentID, certs, anchors, visited)
	if !ok {
		return nil, false
	}

	// 4. Verify the cert with the parent's resolved pub.
	if !e2e.VerifyCert(c, parentPub) {
		return nil, false
	}

	return ed25519.PublicKey(c.ChildSignPub), true
}
