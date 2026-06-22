package main

import (
	"net"
	"testing"
)

func TestEnsureSingleRuntimeFailsWhenPortBusy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String() // already bound

	if _, err := ensureSingleRuntime(addr); err == nil {
		t.Fatal("expected fail-fast when the runtime port is already bound")
	}
}

func TestEnsureSingleRuntimeSucceedsWhenFree(t *testing.T) {
	l2, err := ensureSingleRuntime("127.0.0.1:0")
	if err != nil {
		t.Fatalf("expected success on a free port: %v", err)
	}
	l2.Close()
}
