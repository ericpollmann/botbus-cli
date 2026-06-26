package daemon

import "testing"

func TestReplayWindowMonotonic(t *testing.T) {
	w := newReplayWindow()
	k := replayKey{device: "d", channel: "c", epoch: 1}
	if !w.accept(k, 1) {
		t.Fatal("first counter must be accepted")
	}
	if !w.accept(k, 2) {
		t.Fatal("increasing counter must be accepted")
	}
	if w.accept(k, 2) {
		t.Fatal("duplicate must be rejected")
	}
	if w.accept(k, 1) {
		t.Fatal("out-of-order must be rejected")
	}
}

func TestReplayWindowIndependentKeys(t *testing.T) {
	w := newReplayWindow()
	if !w.accept(replayKey{"a", "c", 1}, 5) {
		t.Fatal("device a accepted")
	}
	if !w.accept(replayKey{"b", "c", 1}, 1) {
		t.Fatal("device b independent")
	}
}
