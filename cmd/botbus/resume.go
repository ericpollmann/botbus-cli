package main

// resume.go — client side of the server's rolling-hash resume protocol.
//
// On every (re)connect the CLI appends ?resume=<count>.<hex-fp> to the
// WebSocket dial URL, where fp is SHA-256 (truncated to 128 bits) over the
// last `count` raw frames it has received, each length-prefixed with a
// uvarint. The server (botbus/resume.go) slides a same-width window over its
// per-channel buffer to find the latest match and replays only the frames
// after it — so a reconnecting client receives the gap it missed, not the
// full last-40 dump it would get with no token.
//
// This algorithm MUST stay byte-identical to the server's hashFrames. The
// pinning test (resume_test.go) guards that by reconstructing the digest
// from stdlib primitives, exactly as the server's own test does.
//
// A fresh client (no frames yet) sends no token and the server falls back to
// last-40 — the intended "recent context on join" behavior.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"strconv"
	"sync"
)

const (
	// resumeDefaultK is the rolling-window depth. Matches the server's
	// recommended client default; the server accepts any 1..64.
	resumeDefaultK = 8
	// resumeFpBytes is the truncated-SHA-256 fingerprint length in bytes
	// (128 bits → 32 hex chars on the wire). Must match botbus/resume.go.
	resumeFpBytes = 16
)

// hashFrames mirrors the server's hashFrames exactly: SHA-256 over the
// concatenation of (uvarint(len(f)) ‖ f) for each frame, truncated to the
// first resumeFpBytes bytes. The length prefix removes frame-boundary
// ambiguity ({"ab","c"} ≠ {"a","bc"}). Client and server MUST agree on this
// byte-for-byte or every resume silently degrades to the last-40 fallback.
func hashFrames(frames [][]byte) [resumeFpBytes]byte {
	h := sha256.New()
	var lenBuf [binary.MaxVarintLen64]byte
	for _, f := range frames {
		n := binary.PutUvarint(lenBuf[:], uint64(len(f)))
		_, _ = h.Write(lenBuf[:n])
		_, _ = h.Write(f)
	}
	sum := h.Sum(nil)
	var out [resumeFpBytes]byte
	copy(out[:], sum[:resumeFpBytes])
	return out
}

// fpRing holds the last k raw frames received on one stream, oldest first.
// It is written by the WS reader goroutine and read at the top of the
// reconnect loop to build the next dial URL's resume token, so it is mutex-
// guarded. The ring persists across reconnects — that is the whole point:
// after a drop it still reflects what the client last saw.
type fpRing struct {
	mu     sync.Mutex
	k      int
	frames [][]byte // ≤ k most-recent frames, oldest first
}

func newFPRing(k int) *fpRing { return &fpRing{k: k} }

// add records one received frame. It copies the bytes because the caller (the
// WS read loop) hands us a slice the websocket library may recycle. Once the
// ring is full it drops the oldest frame, keeping a fixed-size backing array.
func (r *fpRing) add(frame []byte) {
	cp := append([]byte(nil), frame...)
	r.mu.Lock()
	if len(r.frames) < r.k {
		r.frames = append(r.frames, cp)
	} else {
		copy(r.frames, r.frames[1:])
		r.frames[r.k-1] = cp
	}
	r.mu.Unlock()
}

// token returns the "<count>.<hex-fp>" resume token for the frames the ring
// currently holds, or "" if it holds none (a fresh connect — the caller then
// omits the param and the server replays the last 40). count is the actual
// number of frames hashed (≤ k); the server uses it as the window width.
func (r *fpRing) token() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frames) == 0 {
		return ""
	}
	fp := hashFrames(r.frames)
	return strconv.Itoa(len(r.frames)) + "." + hex.EncodeToString(fp[:])
}

// withResume appends ?resume=<token> to a dial URL, or returns it unchanged
// when token is empty (fresh connect) or the URL won't parse. Uses net/url so
// it composes correctly with the "/" and "/audio" stream paths regardless of
// trailing slashes or any future query params.
func withResume(target, token string) string {
	if token == "" {
		return target
	}
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	q := u.Query()
	q.Set("resume", token)
	u.RawQuery = q.Encode()
	return u.String()
}
