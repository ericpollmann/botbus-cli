package e2e_test

// Tests for fabric/e2e — pure crypto primitives.
// All tests are black-box (package e2e_test) to verify the exported surface.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/e2e"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func randKey(t *testing.T) [32]byte {
	t.Helper()
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	return k
}

func randED25519(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// ── Envelope Marshal / Parse ─────────────────────────────────────────────────

func TestEnvelopeMarshalParseRoundTrip(t *testing.T) {
	env := e2e.Envelope{
		Ver:      1,
		KeyEpoch: 42,
		Nonce:    []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		CT:       []byte("ciphertext"),
	}
	b := env.Marshal()
	got, err := e2e.Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Ver != env.Ver || got.KeyEpoch != env.KeyEpoch {
		t.Fatalf("header mismatch: %+v", got)
	}
	if !bytes.Equal(got.Nonce, env.Nonce) {
		t.Fatalf("nonce mismatch: %v", got.Nonce)
	}
	if !bytes.Equal(got.CT, env.CT) {
		t.Fatalf("ct mismatch: %v", got.CT)
	}
}

func TestEnvelopeMarshalLayout(t *testing.T) {
	// Verify exact byte layout: ver‖keyEpoch(4 LE)‖len(nonce)(1)‖nonce‖ct
	env := e2e.Envelope{
		Ver:      0x01,
		KeyEpoch: 0x00000001, // LE: 01 00 00 00
		Nonce:    []byte{0xAA, 0xBB},
		CT:       []byte{0xCC, 0xDD},
	}
	b := env.Marshal()
	// byte 0: Ver=1, bytes 1-4: KeyEpoch LE, byte 5: nonce len=2,
	// bytes 6-7: nonce, bytes 8-9: ct
	want := []byte{0x01, 0x01, 0x00, 0x00, 0x00, 0x02, 0xAA, 0xBB, 0xCC, 0xDD}
	if !bytes.Equal(b, want) {
		t.Fatalf("layout: got % x, want % x", b, want)
	}
}

func TestParseTruncatedInput(t *testing.T) {
	cases := [][]byte{
		{},
		{1},
		{1, 2, 3, 4, 5},          // 5 bytes: shorter than the 6-byte header
		{1, 0, 0, 0, 0, 5, 1, 2}, // header says nonce len=5 but only 2 bytes follow
	}
	for _, c := range cases {
		if _, err := e2e.Parse(c); err == nil {
			t.Errorf("Parse(%x): expected error, got nil", c)
		}
	}
}

func TestParseGarbageInput(t *testing.T) {
	garbage := []byte("not an envelope at all!!")
	if _, err := e2e.Parse(garbage); err == nil {
		t.Error("expected error on garbage input")
	}
}

// ── AEAD Seal / Open ─────────────────────────────────────────────────────────

func TestSealOpenRoundTrip(t *testing.T) {
	key := randKey(t)
	plaintext := []byte("hello world")
	aad := []byte("channel-id:epoch=1")

	env, err := e2e.Seal(key, 1, aad, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(env.Nonce) != 24 { // XChaCha20-Poly1305 NonceSizeX
		t.Fatalf("nonce len: got %d, want 24", len(env.Nonce))
	}

	got, err := e2e.Open(key, aad, env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch: %q vs %q", got, plaintext)
	}
}

func TestOpenWrongKey(t *testing.T) {
	key := randKey(t)
	env, _ := e2e.Seal(key, 1, nil, []byte("secret"))
	wrongKey := randKey(t)
	if _, err := e2e.Open(wrongKey, nil, env); err == nil {
		t.Fatal("expected auth failure on wrong key")
	}
}

func TestOpenFlippedBit(t *testing.T) {
	key := randKey(t)
	env, _ := e2e.Seal(key, 1, nil, []byte("secret"))
	env.CT[0] ^= 0xFF
	if _, err := e2e.Open(key, nil, env); err == nil {
		t.Fatal("expected auth failure on tampered ciphertext")
	}
}

func TestOpenWrongAAD(t *testing.T) {
	key := randKey(t)
	env, _ := e2e.Seal(key, 1, []byte("aad-1"), []byte("secret"))
	if _, err := e2e.Open(key, []byte("aad-2"), env); err == nil {
		t.Fatal("expected auth failure on wrong AAD")
	}
}

// TestOpenBadNonceLengthNoPanic: a relay-controlled wrong-length nonce must
// yield an error, never a panic (aead.Open panics on a bad nonce length).
func TestOpenBadNonceLengthNoPanic(t *testing.T) {
	key := randKey(t)
	env, _ := e2e.Seal(key, 1, nil, []byte("x"))
	env.Nonce = env.Nonce[:8] // truncate to an invalid length
	if _, err := e2e.Open(key, nil, env); err == nil {
		t.Fatal("expected error on invalid nonce length")
	}
}

func TestSealNonceUniqueness(t *testing.T) {
	key := randKey(t)
	plaintext := []byte("same plaintext")
	e1, _ := e2e.Seal(key, 1, nil, plaintext)
	e2v, _ := e2e.Seal(key, 1, nil, plaintext)
	if bytes.Equal(e1.Nonce, e2v.Nonce) {
		t.Fatal("two Seals produced identical nonces")
	}
	if bytes.Equal(e1.CT, e2v.CT) {
		t.Fatal("two Seals of same plaintext produced identical ciphertexts")
	}
}

// ── SealMessage / OpenMessage ────────────────────────────────────────────────

func TestSealMessageOpenMessageRoundTrip(t *testing.T) {
	wsKey := randKey(t)
	pub, priv := randED25519(t)
	channelID := "ch-abc"
	deviceID := "device-1"

	lookup := func(id string) (ed25519.PublicKey, bool) {
		if id == deviceID {
			return pub, true
		}
		return nil, false
	}

	env, err := e2e.SealMessage(wsKey, 1, channelID, deviceID, priv, 42, []byte("payload"))
	if err != nil {
		t.Fatalf("SealMessage: %v", err)
	}

	gotDevice, gotCounter, gotPlain, err := e2e.OpenMessage(wsKey, channelID, env, lookup)
	if err != nil {
		t.Fatalf("OpenMessage: %v", err)
	}
	if gotDevice != deviceID {
		t.Fatalf("deviceID: got %q, want %q", gotDevice, deviceID)
	}
	if gotCounter != 42 {
		t.Fatalf("counter: got %d, want 42", gotCounter)
	}
	if !bytes.Equal(gotPlain, []byte("payload")) {
		t.Fatalf("plaintext mismatch: %q", gotPlain)
	}
}

func TestOpenMessageWrongWorkspaceKey(t *testing.T) {
	wsKey := randKey(t)
	_, priv := randED25519(t)
	env, _ := e2e.SealMessage(wsKey, 1, "ch", "dev", priv, 1, []byte("data"))
	wrongKey := randKey(t)
	lookup := func(id string) (ed25519.PublicKey, bool) { return nil, false }
	if _, _, _, err := e2e.OpenMessage(wrongKey, "ch", env, lookup); err == nil {
		t.Fatal("expected failure with wrong workspace key")
	}
}

func TestOpenMessageUnknownDevice(t *testing.T) {
	wsKey := randKey(t)
	_, priv := randED25519(t)
	env, _ := e2e.SealMessage(wsKey, 1, "ch", "dev-unknown", priv, 1, []byte("data"))
	lookup := func(id string) (ed25519.PublicKey, bool) { return nil, false }
	if _, _, _, err := e2e.OpenMessage(wsKey, "ch", env, lookup); err == nil {
		t.Fatal("expected failure on unknown device")
	}
}

func TestOpenMessageTamperedPlaintext(t *testing.T) {
	wsKey := randKey(t)
	pub, priv := randED25519(t)
	lookup := func(id string) (ed25519.PublicKey, bool) { return pub, true }
	env, _ := e2e.SealMessage(wsKey, 1, "ch", "dev", priv, 1, []byte("original"))
	// Flip a byte in the CT (which contains the inner signed payload).
	env.CT[0] ^= 0x01
	if _, _, _, err := e2e.OpenMessage(wsKey, "ch", env, lookup); err == nil {
		t.Fatal("expected failure on tampered ciphertext")
	}
}

func TestOpenMessageWrongChannelID(t *testing.T) {
	wsKey := randKey(t)
	pub, priv := randED25519(t)
	lookup := func(id string) (ed25519.PublicKey, bool) { return pub, true }
	env, _ := e2e.SealMessage(wsKey, 1, "ch-correct", "dev", priv, 1, []byte("data"))
	// Opening with a different channelID changes the AAD → AEAD failure.
	if _, _, _, err := e2e.OpenMessage(wsKey, "ch-wrong", env, lookup); err == nil {
		t.Fatal("expected failure on wrong channelID")
	}
}

func TestOpenMessageWrongEpoch(t *testing.T) {
	// Seal under epoch 1, then manually change KeyEpoch in the envelope to 2
	// before opening. The AAD includes the epoch so AEAD will reject it.
	wsKey := randKey(t)
	pub, priv := randED25519(t)
	lookup := func(id string) (ed25519.PublicKey, bool) { return pub, true }
	env, _ := e2e.SealMessage(wsKey, 1, "ch", "dev", priv, 1, []byte("data"))
	env.KeyEpoch = 2
	if _, _, _, err := e2e.OpenMessage(wsKey, "ch", env, lookup); err == nil {
		t.Fatal("expected failure on wrong epoch")
	}
}

// ── FORGERY test (headline guarantee) ────────────────────────────────────────
//
// A party holding the workspace key but NOT deviceB's private key must not be
// able to produce an Envelope that OpenMessage attributes to deviceB.
// Strategy: seal as deviceA, then look up the claimed id against deviceB's pubkey.
// This confirms confidentiality ≠ authorship.

func TestForgeryDeviceACannotImpersonateDeviceB(t *testing.T) {
	wsKey := randKey(t)
	_, privA := randED25519(t)
	pubB, _ := randED25519(t)

	// deviceA (attacker) seals claiming id "device-A"
	env, err := e2e.SealMessage(wsKey, 1, "ch", "device-A", privA, 1, []byte("forge"))
	if err != nil {
		t.Fatalf("SealMessage: %v", err)
	}

	// Lookup always returns deviceB's pubkey regardless of claimed id.
	// This simulates: the victim knows the claimed device but maps it to B's pubkey.
	forgedLookup := func(id string) (ed25519.PublicKey, bool) {
		return pubB, true
	}

	_, _, _, err = e2e.OpenMessage(wsKey, "ch", env, forgedLookup)
	if err == nil {
		t.Fatal("FORGERY: OpenMessage accepted a message attributed to a device whose private key was never used")
	}
}

// ── HKDF KAT (RFC 5869 Appendix A, Test Case 1) ──────────────────────────────
//
// IKM  = 0x0b0b...0b (22 bytes)
// salt = 0x000102...0c (13 bytes)
// info = 0xf0f1...f9 (10 bytes)
// L    = 42
// OKM  = 0x3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865
//
// Source: https://www.rfc-editor.org/rfc/rfc5869#appendix-A

func TestHKDFKnownAnswer(t *testing.T) {
	ikm := mustDecodeHex(t, "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	salt := mustDecodeHex(t, "000102030405060708090a0b0c")
	info := mustDecodeHex(t, "f0f1f2f3f4f5f6f7f8f9")
	wantOKM := mustDecodeHex(t, "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865")

	got := e2e.HKDF(ikm, salt, info, 42)
	if !bytes.Equal(got, wantOKM) {
		t.Fatalf("HKDF KAT failed:\n got: %x\nwant: %x", got, wantOKM)
	}
}

// RFC 5869 Test Case 2 (no salt, longer OKM)
func TestHKDFKnownAnswerCase2(t *testing.T) {
	ikm := mustDecodeHex(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f")
	salt := mustDecodeHex(t, "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeaf")
	info := mustDecodeHex(t, "b0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeeff0f1f2f3f4f5f6f7f8f9fafbfcfdfeff")
	wantOKM := mustDecodeHex(t, "b11e398dc80327a1c8e7f78c596a49344f012eda2d4efad8a050cc4c19afa97c59045a99cac7827271cb41c65e590e09da3275600c2f09b8367793a9aca3db71cc30c58179ec3e87c14c01d5c1f3434f1d87")

	got := e2e.HKDF(ikm, salt, info, 82)
	if !bytes.Equal(got, wantOKM) {
		t.Fatalf("HKDF KAT case 2 failed:\n got: %x\nwant: %x", got, wantOKM)
	}
}

// RFC 5869 Test Case 3 (zero-length salt, zero-length info)
func TestHKDFKnownAnswerCase3(t *testing.T) {
	ikm := mustDecodeHex(t, "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	// No salt: HKDF spec says use HashLen zeros when salt is not provided.
	// RFC 5869 test case 3: salt = not provided (nil in our API = HashLen zeros).
	info := []byte{}
	wantOKM := mustDecodeHex(t, "8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8")

	got := e2e.HKDF(ikm, nil, info, 42)
	if !bytes.Equal(got, wantOKM) {
		t.Fatalf("HKDF KAT case 3 failed:\n got: %x\nwant: %x", got, wantOKM)
	}
}

// ── DeriveChannelID ───────────────────────────────────────────────────────────

func TestDeriveChannelIDDeterminism(t *testing.T) {
	key := randKey(t)
	salt := []byte("per-epoch-salt")
	a := e2e.DeriveChannelID(key, "ws-1", "reviews", 1, salt)
	b := e2e.DeriveChannelID(key, "ws-1", "reviews", 1, salt)
	if !bytes.Equal(a, b) {
		t.Fatal("DeriveChannelID not deterministic")
	}
	if len(a) != 32 {
		t.Fatalf("expected 32-byte output, got %d", len(a))
	}
}

func TestDeriveChannelIDDifferentInputsDifferentOutputs(t *testing.T) {
	key := randKey(t)
	salt := []byte("salt")

	base := e2e.DeriveChannelID(key, "ws-1", "reviews", 1, salt)

	cases := []struct {
		name string
		id   []byte
	}{
		{"different key", func() []byte { k2 := randKey(t); return e2e.DeriveChannelID(k2, "ws-1", "reviews", 1, salt) }()},
		{"different wsID", e2e.DeriveChannelID(key, "ws-2", "reviews", 1, salt)},
		{"different topic", e2e.DeriveChannelID(key, "ws-1", "docs", 1, salt)},
		{"different epoch", e2e.DeriveChannelID(key, "ws-1", "reviews", 2, salt)},
		{"different salt", e2e.DeriveChannelID(key, "ws-1", "reviews", 1, []byte("other"))},
	}
	for _, tc := range cases {
		if bytes.Equal(base, tc.id) {
			t.Errorf("DeriveChannelID: %s produced the same output as base", tc.name)
		}
	}
}

// TestDeriveChannelIDDelimiterInjection: length-prefixed info must make
// (workspaceID, topic) pairs that differ only by where a delimiter "lands"
// derive DISTINCT ids — otherwise channel isolation across workspaces breaks.
func TestDeriveChannelIDDelimiterInjection(t *testing.T) {
	key := randKey(t)
	salt := []byte("s")
	// These collided under the old "|"-joined info string.
	a := e2e.DeriveChannelID(key, "ws1|chan|t1", "x", 1, salt)
	b := e2e.DeriveChannelID(key, "ws1", "t1|chan|x", 1, salt)
	if bytes.Equal(a, b) {
		t.Fatal("delimiter-injection: distinct (workspaceID, topic) pairs collided")
	}
}

func TestDeriveChannelIDDifferentTopicsSameParams(t *testing.T) {
	key := randKey(t)
	salt := []byte("s")
	reviews := e2e.DeriveChannelID(key, "ws", "reviews", 1, salt)
	docs := e2e.DeriveChannelID(key, "ws", "docs", 1, salt)
	if bytes.Equal(reviews, docs) {
		t.Fatal("different topics under same key/epoch must not collide")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex decode %q: %v", s, err)
	}
	return b
}
