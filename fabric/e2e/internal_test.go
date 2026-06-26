package e2e

// White-box tests for internal helpers. These live in package e2e (not e2e_test)
// so they can reach unexported functions directly.

import (
	"bytes"
	"testing"
)

func TestGetLenPrefixedBytesShortPrefix(t *testing.T) {
	_, _, err := getLenPrefixedBytes([]byte{1, 2}) // only 2 bytes, need 4 for length
	if err == nil {
		t.Fatal("expected error on truncated length prefix")
	}
}

func TestGetLenPrefixedBytesShortData(t *testing.T) {
	// length prefix says 10 bytes but only 2 bytes follow
	buf := []byte{10, 0, 0, 0, 0xAA, 0xBB} // len=10, data=[AA BB]
	_, _, err := getLenPrefixedBytes(buf)
	if err == nil {
		t.Fatal("expected error when data shorter than declared length")
	}
}

func TestGetLenPrefixedBytesRoundTrip(t *testing.T) {
	payload := []byte("hello, world")
	buf := putLenPrefixedBytes(nil, payload)
	got, rest, err := getLenPrefixedBytes(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: %v", rest)
	}
}

func TestOpenUnsupportedAlg(t *testing.T) {
	var key [32]byte
	env := Envelope{Ver: 1, Alg: 2, KeyEpoch: 0, Nonce: make([]byte, 12), CT: []byte("ct")}
	if _, err := Open(key, nil, env); err == nil {
		t.Fatal("expected error for unsupported alg=2")
	}
}

func TestDecodeInnerTrailingBytes(t *testing.T) {
	// Encode a valid inner then append extra bytes.
	inner := encodeInner("dev", 1, []byte("plain"), []byte("sig"))
	inner = append(inner, 0xFF) // trailing garbage
	_, _, _, _, err := decodeInner(inner)
	if err == nil {
		t.Fatal("expected error on trailing bytes in inner")
	}
}

func TestDecodeInnerCounterTruncated(t *testing.T) {
	// Build an inner with a valid deviceID prefix but truncate before the counter.
	buf := putLenPrefixedString(nil, "dev-x")
	buf = append(buf, 0xAA, 0xBB, 0xCC) // only 3 bytes where counter needs 8
	_, _, _, _, err := decodeInner(buf)
	if err == nil {
		t.Fatal("expected error on truncated counter")
	}
}
