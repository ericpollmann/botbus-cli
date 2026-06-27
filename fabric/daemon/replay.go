package daemon

import "sync"

type replayKey struct {
	device  string
	channel string
	epoch   uint32
}

type replayWindow struct {
	mu       sync.Mutex
	lastSeen map[replayKey]uint64
}

func newReplayWindow() *replayWindow {
	return &replayWindow{lastSeen: make(map[replayKey]uint64)}
}

// accept reports whether counter is fresh (strictly greater than the last seen
// for k) and records it. Duplicates and out-of-order counters are rejected.
func (w *replayWindow) accept(k replayKey, counter uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if counter <= w.lastSeen[k] { // zero-value 0 => first counter >=1 passes
		return false
	}
	w.lastSeen[k] = counter
	return true
}
