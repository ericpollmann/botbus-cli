package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestHashFramesMatchesServerAlgorithm pins the algorithm by reconstructing
// the digest from stdlib primitives — identical in shape to the server's
// TestHashFramesMatchesRawSHA256 (botbus/resume_test.go). If either side
// drifts (drops the length prefix, changes the truncation slice, swaps the
// hash), this fails. This is the primary cross-repo correctness guard.
func TestHashFramesMatchesServerAlgorithm(t *testing.T) {
	frames := [][]byte{[]byte("alpha"), []byte("beta")}
	h := sha256.New()
	var lenBuf [binary.MaxVarintLen64]byte
	for _, f := range frames {
		n := binary.PutUvarint(lenBuf[:], uint64(len(f)))
		h.Write(lenBuf[:n])
		h.Write(f)
	}
	want := h.Sum(nil)[:resumeFpBytes]
	got := hashFrames(frames)
	if !bytesEqual(got[:], want) {
		t.Errorf("got %x, want %x (algorithm drifted from server)", got[:], want)
	}
}

// TestHashFramesGolden is a hardcoded reference value. The same input must
// produce this exact fingerprint on the server too (botbus/resume.go uses the
// identical algorithm). If a refactor changes the hash, this magic value
// changes — a deliberate tripwire that forces a conscious cross-repo update.
func TestHashFramesGolden(t *testing.T) {
	got := hashFrames([][]byte{[]byte("alice: hi"), []byte("bob: yo")})
	const want = "90ba3c72cc5e940ff3a1f41f76b25799"
	if hex.EncodeToString(got[:]) != want {
		t.Errorf("golden mismatch: got %s want %s", hex.EncodeToString(got[:]), want)
	}
}

func TestHashFramesDeterministic(t *testing.T) {
	a := hashFrames([][]byte{[]byte("hello"), []byte("world")})
	b := hashFrames([][]byte{[]byte("hello"), []byte("world")})
	if a != b {
		t.Fatalf("not deterministic: %x vs %x", a[:], b[:])
	}
}

func TestHashFramesLengthPrefixDisambiguates(t *testing.T) {
	// {"ab","c"} and {"a","bc"} must hash differently — the whole point of
	// the uvarint length prefix.
	a := hashFrames([][]byte{[]byte("ab"), []byte("c")})
	b := hashFrames([][]byte{[]byte("a"), []byte("bc")})
	if a == b {
		t.Fatalf("length prefix failed to disambiguate: both %x", a[:])
	}
}

func TestFPRingTokenEmpty(t *testing.T) {
	r := newFPRing(resumeDefaultK)
	if tok := r.token(); tok != "" {
		t.Errorf("empty ring should yield empty token, got %q", tok)
	}
}

func TestFPRingTokenCountMatchesFrames(t *testing.T) {
	r := newFPRing(resumeDefaultK)
	r.add([]byte("a"))
	r.add([]byte("b"))
	r.add([]byte("c"))
	tok := r.token()
	if !strings.HasPrefix(tok, "3.") {
		t.Errorf("3 frames → token should start \"3.\", got %q", tok)
	}
	// fp is 32 hex chars after the "3." prefix.
	if len(tok) != len("3.")+2*resumeFpBytes {
		t.Errorf("unexpected token length: %q", tok)
	}
}

func TestFPRingBoundedAtK(t *testing.T) {
	r := newFPRing(3) // small k to exercise the drop-oldest path
	for i := 0; i < 6; i++ {
		r.add([]byte{byte('0' + i)}) // "0","1",...,"5"
	}
	tok := r.token()
	if !strings.HasPrefix(tok, "3.") {
		t.Errorf("k=3 ring should cap at 3 frames, token=%q", tok)
	}
	// Token must reflect the LAST 3 frames ("3","4","5"), not the first.
	wantFp := hashFrames([][]byte{[]byte("3"), []byte("4"), []byte("5")})
	want := "3." + hex.EncodeToString(wantFp[:])
	if tok != want {
		t.Errorf("ring did not keep the newest 3 frames: got %q want %q", tok, want)
	}
}

func TestFPRingAddCopies(t *testing.T) {
	// The reader hands us a slice the websocket lib may recycle; add must copy.
	r := newFPRing(resumeDefaultK)
	frame := []byte("hello")
	r.add(frame)
	before := r.token()
	copy(frame, "XXXXX") // mutate the caller's slice after add returned
	after := r.token()
	if before != after {
		t.Errorf("add did not copy the frame: token changed %q → %q", before, after)
	}
}

// TestFPRingTokenMatchesServerWindow is the resume-correctness anchor: after a
// client receives frames f0..fN, the token it sends must equal
// hashFrames(last k of those) — which is exactly the window the server's
// findResumeMatch slides over its buffer. If these match, the server finds the
// client's position and replays only the gap.
func TestFPRingTokenMatchesServerWindow(t *testing.T) {
	k := resumeDefaultK
	r := newFPRing(k)
	var all [][]byte
	for i := 0; i < 20; i++ {
		f := []byte{byte(i)}
		all = append(all, f)
		r.add(f)
	}
	window := all[len(all)-k:] // the last k frames, oldest first
	wantFp := hashFrames(window)
	want := strconv.Itoa(k) + "." + hex.EncodeToString(wantFp[:])
	if got := r.token(); got != want {
		t.Errorf("token %q does not match server window hash %q", got, want)
	}
}

func TestWithResumeEmptyToken(t *testing.T) {
	target := "wss://abc.botbus.ai/"
	if got := withResume(target, ""); got != target {
		t.Errorf("empty token should leave URL unchanged, got %q", got)
	}
}

func TestWithResumeTextPath(t *testing.T) {
	got := withResume("wss://abc.botbus.ai/", "8.deadbeef")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result not parseable: %v", err)
	}
	if u.Path != "/" {
		t.Errorf("path mangled: %q", u.Path)
	}
	if u.Query().Get("resume") != "8.deadbeef" {
		t.Errorf("resume param missing/wrong in %q", got)
	}
}

func TestWithResumeAudioPath(t *testing.T) {
	got := withResume("wss://abc.botbus.ai/audio", "8.deadbeef")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result not parseable: %v", err)
	}
	if u.Path != "/audio" {
		t.Errorf("audio path mangled: %q", u.Path)
	}
	if u.Query().Get("resume") != "8.deadbeef" {
		t.Errorf("resume param missing/wrong in %q", got)
	}
}

func TestWithResumeMalformedURLFallsBack(t *testing.T) {
	// A control character makes url.Parse error; withResume returns the input
	// unchanged rather than dropping the connection.
	bad := "wss://abc.botbus.ai/\x7f"
	if got := withResume(bad, "8.deadbeef"); got != bad {
		t.Errorf("malformed URL should pass through unchanged, got %q", got)
	}
}

// bytesEqual is a tiny local helper (avoids importing bytes just for one call).
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
