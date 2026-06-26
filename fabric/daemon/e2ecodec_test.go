package daemon

import "testing"

func TestContentCodecRoundTrip(t *testing.T) {
	b := encodeContent("hello", "world body")
	s, body, err := decodeContent(b)
	if err != nil {
		t.Fatal(err)
	}
	if s != "hello" || body != "world body" {
		t.Fatalf("got (%q,%q)", s, body)
	}
}

func TestDecodeContentRejectsGarbage(t *testing.T) {
	if _, _, err := decodeContent([]byte("not json")); err == nil {
		t.Fatal("expected error on garbage")
	}
}
