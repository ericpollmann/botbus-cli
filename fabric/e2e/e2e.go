// Package e2e provides pure cryptographic primitives for botbus end-to-end
// encryption. It has no I/O, no network, and no external dependencies —
// stdlib only.
//
// AEAD algorithm notes:
//   - Alg=1: AES-256-GCM with a 12-byte (96-bit) random nonce.
//     At-scale note: GCM nonce collision probability is ~2^(-32) at 2^32 seals
//     with random nonces (birthday bound). For high-volume channels rotate the
//     workspace key epoch well before reaching that bound.
//   - Alg=2 (deferred): XChaCha20-Poly1305 with a 24-byte nonce. This is the
//     spec's preferred AEAD (192-bit nonce eliminates the GCM volume concern)
//     but requires golang.org/x/crypto, which is not in go.mod. The versioned
//     Alg field in Envelope makes it a clean future addition; existing Alg=1
//     ciphertexts remain openable.
//
// # Envelope binary layout
//
//	byte  0     : Ver (uint8)
//	byte  1     : Alg (uint8)
//	bytes 2–5   : KeyEpoch (uint32, little-endian)
//	byte  6     : len(Nonce) (uint8)
//	bytes 7…    : Nonce (len bytes)
//	bytes 7+len…: CT (remainder; GCM appends its 16-byte auth tag to CT)
//
// # SealMessage inner encoding
//
//	The inner plaintext that is AEAD-encrypted:
//	  [4]  len(deviceID) as uint32 LE
//	  [n]  deviceID bytes
//	  [8]  counter as uint64 LE
//	  [4]  len(plaintext) as uint32 LE
//	  [n]  plaintext bytes
//	  [4]  len(sig) as uint32 LE   (always 64 for Ed25519, but length-prefixed for safety)
//	  [n]  sig bytes
//
//	The signature is computed over (a fixed domain tag prevents cross-protocol
//	reuse of a device key; deviceID is bound so the signed statement is
//	self-contained — "device D, on channel C epoch E counter N, says P"):
//	  "botbus-e2e-msg-v1\x00"               (fixed domain tag)
//	  [4]  len(channelID) as uint32 LE
//	  [n]  channelID bytes
//	  [4]  len(deviceID) as uint32 LE
//	  [n]  deviceID bytes
//	  [4]  keyEpoch as uint32 LE
//	  [8]  counter as uint64 LE
//	  [4]  len(plaintext) as uint32 LE
//	  [n]  plaintext bytes
//
//	AAD for the outer AEAD = channelID‖keyEpoch(uint32 LE).
//	This binds the ciphertext to the channel and epoch so replay across
//	channels or across key epochs is rejected by the AEAD.
//
// # Canonicalization note
//
//	All multi-byte integers use little-endian. All variable-length fields are
//	length-prefixed with a uint32 LE length. There is no ambiguity: every
//	field is either fixed-size or preceded by its length. The encoding is not
//	self-describing beyond the Envelope header fields; the schema is in this
//	comment and must not change without a Ver bump.
package e2e

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// ── Envelope ─────────────────────────────────────────────────────────────────

// Envelope is a versioned, authenticated ciphertext envelope.
type Envelope struct {
	Ver      uint8
	Alg      uint8
	KeyEpoch uint32
	Nonce    []byte
	CT       []byte // for AES-256-GCM: ciphertext ‖ 16-byte GCM tag
}

// Marshal serialises the envelope to bytes. See package doc for layout.
func (e Envelope) Marshal() []byte {
	nl := len(e.Nonce)
	if nl > 255 {
		// The length field is a single byte; a longer nonce would be silently
		// truncated and mis-parsed. AEADs here use ≤24-byte nonces, so this is
		// a programmer error.
		panic("e2e: nonce too long to marshal (>255)")
	}
	buf := make([]byte, 7+nl+len(e.CT))
	buf[0] = e.Ver
	buf[1] = e.Alg
	binary.LittleEndian.PutUint32(buf[2:6], e.KeyEpoch)
	buf[6] = uint8(nl)
	copy(buf[7:], e.Nonce)
	copy(buf[7+nl:], e.CT)
	return buf
}

// Parse deserialises an Envelope, rejecting malformed or truncated input.
func Parse(b []byte) (Envelope, error) {
	if len(b) < 7 {
		return Envelope{}, errors.New("e2e: envelope too short")
	}
	nl := int(b[6])
	if len(b) < 7+nl {
		return Envelope{}, fmt.Errorf("e2e: envelope nonce truncated (need %d, have %d)", nl, len(b)-7)
	}
	e := Envelope{
		Ver:      b[0],
		Alg:      b[1],
		KeyEpoch: binary.LittleEndian.Uint32(b[2:6]),
		Nonce:    make([]byte, nl),
		CT:       make([]byte, len(b)-7-nl),
	}
	copy(e.Nonce, b[7:7+nl])
	copy(e.CT, b[7+nl:])
	return e, nil
}

// ── AEAD ─────────────────────────────────────────────────────────────────────

// Seal encrypts plaintext with AES-256-GCM (Alg=1) using a fresh 12-byte
// random nonce. aad is passed as GCM additional authenticated data.
func Seal(key [32]byte, keyEpoch uint32, aad, plaintext []byte) (Envelope, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return Envelope{}, fmt.Errorf("e2e: AES init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, fmt.Errorf("e2e: GCM init: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, fmt.Errorf("e2e: nonce gen: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad)
	return Envelope{Ver: 1, Alg: 1, KeyEpoch: keyEpoch, Nonce: nonce, CT: ct}, nil
}

// Open decrypts and authenticates an Envelope sealed with Seal.
// Returns a clear error on authentication failure without leaking oracle info.
func Open(key [32]byte, aad []byte, e Envelope) ([]byte, error) {
	if e.Alg != 1 {
		return nil, fmt.Errorf("e2e: unsupported alg %d", e.Alg)
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("e2e: AES init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("e2e: GCM init: %w", err)
	}
	// The nonce comes off the wire (relay-controlled); gcm.Open PANICS on a
	// wrong-length nonce, so validate before calling it — else a crafted
	// nonce-len byte is a remotely-triggerable crash.
	if len(e.Nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("e2e: invalid nonce length %d", len(e.Nonce))
	}
	plain, err := gcm.Open(nil, e.Nonce, e.CT, aad)
	if err != nil {
		return nil, errors.New("e2e: authentication failed")
	}
	return plain, nil
}

// ── SealMessage / OpenMessage ─────────────────────────────────────────────────

// SealMessage implements sign-then-encrypt for authenticated authorship.
//
// It signs the message inside the encryption layer so the relay cannot see
// which device signed. The AAD binds the ciphertext to a specific channel and
// key epoch so cross-channel or cross-epoch replay is rejected by the AEAD.
//
// Monotonic-counter enforcement (rejecting duplicates and out-of-window
// counters) is the caller's responsibility; this function only round-trips
// the counter faithfully.
func SealMessage(
	workspaceKey [32]byte,
	keyEpoch uint32,
	channelID, deviceID string,
	devPriv ed25519.PrivateKey,
	counter uint64,
	plaintext []byte,
) (Envelope, error) {
	// 1. Build the signed blob: domain-tag ‖ channelID ‖ deviceID ‖ keyEpoch ‖ counter ‖ plaintext
	sig := ed25519.Sign(devPriv, signedPayload(channelID, deviceID, keyEpoch, counter, plaintext))

	// 2. Encode inner = {deviceID, counter, plaintext, sig} — all length-prefixed.
	inner := encodeInner(deviceID, counter, plaintext, sig)

	// 3. AAD = channelID ‖ keyEpoch (LE) — binds to channel+epoch.
	aad := channelAAD(channelID, keyEpoch)

	return Seal(workspaceKey, keyEpoch, aad, inner)
}

// OpenMessage decrypts and verifies a message sealed by SealMessage.
//
// It returns the sender's deviceID, the monotonic counter, and the plaintext.
// lookupPub must return the Ed25519 public key for a given deviceID; returning
// (nil, false) causes OpenMessage to return an error (unknown device).
func OpenMessage(
	workspaceKey [32]byte,
	channelID string,
	e Envelope,
	lookupPub func(deviceID string) (ed25519.PublicKey, bool),
) (deviceID string, counter uint64, plaintext []byte, err error) {
	aad := channelAAD(channelID, e.KeyEpoch)
	inner, err := Open(workspaceKey, aad, e)
	if err != nil {
		return "", 0, nil, err
	}
	deviceID, counter, plaintext, sig, err := decodeInner(inner)
	if err != nil {
		return "", 0, nil, fmt.Errorf("e2e: inner decode: %w", err)
	}
	pub, ok := lookupPub(deviceID)
	if !ok {
		return "", 0, nil, fmt.Errorf("e2e: unknown device %q", deviceID)
	}
	if !ed25519.Verify(pub, signedPayload(channelID, deviceID, e.KeyEpoch, counter, plaintext), sig) {
		return "", 0, nil, errors.New("e2e: signature verification failed")
	}
	return deviceID, counter, plaintext, nil
}

// ── HKDF (RFC 5869, HMAC-SHA256) ─────────────────────────────────────────────

// HKDF performs RFC 5869 Extract-then-Expand with HMAC-SHA256.
// If salt is nil or empty, it defaults to a string of HashLen zero bytes per
// the spec (section 2.2).
func HKDF(secret, salt, info []byte, length int) []byte {
	if length < 0 || length > 255*sha256.Size {
		// RFC 5869 §2.3 caps L at 255*HashLen; beyond it the 1-byte block
		// counter wraps and the output repeats. Our uses ask for 32 bytes.
		panic("e2e: HKDF length out of range (0..255*HashLen)")
	}
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	// Extract
	h := hmac.New(sha256.New, salt)
	h.Write(secret)
	prk := h.Sum(nil)

	// Expand
	okm := make([]byte, 0, length)
	var prev []byte
	for i := 1; len(okm) < length; i++ {
		h = hmac.New(sha256.New, prk)
		h.Write(prev)
		h.Write(info)
		h.Write([]byte{byte(i)})
		prev = h.Sum(nil)
		okm = append(okm, prev...)
	}
	return okm[:length]
}

// DeriveChannelID derives a 32-byte channel identifier using HKDF.
//
// info is domain-separated, version-bound, and INJECTION-SAFE: a fixed tag
// followed by length-prefixed workspaceID and topic, then the epoch as 4 LE
// bytes. Length-prefixing (rather than "|"-joining) is essential — otherwise a
// workspaceID/topic containing the delimiter could collide two distinct
// (workspaceID, topic) pairs onto the same id and break channel isolation.
//
// salt is mixed in as the HKDF salt (use a per-epoch random salt so that a
// future key leak cannot recompute historical ids).
//
// NOTE: encoding these bytes into a hub-acceptable channel-id string (the hub
// validates ids via its own HMAC-checksum format) is deferred to integration
// (Phase 3). This function returns raw 32-byte key material.
func DeriveChannelID(workspaceKey [32]byte, workspaceID, topic string, epoch uint32, salt []byte) []byte {
	info := append([]byte(nil), "botbus/v1/chan\x00"...)
	info = putLenPrefixedString(info, workspaceID)
	info = putLenPrefixedString(info, topic)
	var ep [4]byte
	binary.LittleEndian.PutUint32(ep[:], epoch)
	info = append(info, ep[:]...)
	return HKDF(workspaceKey[:], salt, info, 32)
}

// ── internal helpers ──────────────────────────────────────────────────────────

// signedPayload builds the canonical byte string that is signed / verified.
// A fixed domain tag prevents a device's Ed25519 key from being cross-used to
// sign other protocol structures; deviceID is bound so the signed statement is
// self-contained (no reliance on which pubkey happens to verify).
// Layout: tag ‖ channelID ‖ deviceID ‖ keyEpoch(4 LE) ‖ counter(8 LE) ‖ plaintext(len-prefixed)
func signedPayload(channelID, deviceID string, keyEpoch uint32, counter uint64, plaintext []byte) []byte {
	buf := append([]byte(nil), "botbus-e2e-msg-v1\x00"...)
	buf = putLenPrefixedString(buf, channelID)
	buf = putLenPrefixedString(buf, deviceID)
	var tmp [8]byte
	binary.LittleEndian.PutUint32(tmp[:4], keyEpoch)
	buf = append(buf, tmp[:4]...)
	binary.LittleEndian.PutUint64(tmp[:], counter)
	buf = append(buf, tmp[:]...)
	buf = putLenPrefixedBytes(buf, plaintext)
	return buf
}

// channelAAD builds the AAD for the outer AEAD: channelID ‖ keyEpoch(4 LE).
func channelAAD(channelID string, keyEpoch uint32) []byte {
	buf := putLenPrefixedString(nil, channelID)
	var ep [4]byte
	binary.LittleEndian.PutUint32(ep[:], keyEpoch)
	return append(buf, ep[:]...)
}

// encodeInner length-prefix-encodes the inner message fields.
func encodeInner(deviceID string, counter uint64, plaintext, sig []byte) []byte {
	buf := putLenPrefixedString(nil, deviceID)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], counter)
	buf = append(buf, tmp[:]...)
	buf = putLenPrefixedBytes(buf, plaintext)
	buf = putLenPrefixedBytes(buf, sig)
	return buf
}

// decodeInner reverses encodeInner.
func decodeInner(b []byte) (deviceID string, counter uint64, plaintext, sig []byte, err error) {
	var devBytes []byte
	devBytes, b, err = getLenPrefixedBytes(b)
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("deviceID: %w", err)
	}
	deviceID = string(devBytes)

	if len(b) < 8 {
		return "", 0, nil, nil, errors.New("counter truncated")
	}
	counter = binary.LittleEndian.Uint64(b[:8])
	b = b[8:]

	plaintext, b, err = getLenPrefixedBytes(b)
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("plaintext: %w", err)
	}
	sig, b, err = getLenPrefixedBytes(b)
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("sig: %w", err)
	}
	if len(b) != 0 {
		return "", 0, nil, nil, errors.New("trailing bytes in inner")
	}
	return deviceID, counter, plaintext, sig, nil
}

// putLenPrefixedString appends a uint32-LE length-prefixed string to dst.
func putLenPrefixedString(dst []byte, s string) []byte {
	return putLenPrefixedBytes(dst, []byte(s))
}

// putLenPrefixedBytes appends a uint32-LE length-prefixed byte slice to dst.
func putLenPrefixedBytes(dst, b []byte) []byte {
	var lbuf [4]byte
	binary.LittleEndian.PutUint32(lbuf[:], uint32(len(b)))
	dst = append(dst, lbuf[:]...)
	return append(dst, b...)
}

// getLenPrefixedBytes reads a uint32-LE length-prefixed byte slice from b,
// returning the slice and the remaining bytes.
func getLenPrefixedBytes(b []byte) (data, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errors.New("length prefix truncated")
	}
	l := int(binary.LittleEndian.Uint32(b[:4]))
	b = b[4:]
	if l < 0 || len(b) < l { // l<0 guards a >2GiB prefix on 32-bit int
		return nil, nil, fmt.Errorf("data truncated (need %d, have %d)", l, len(b))
	}
	return b[:l], b[l:], nil
}
