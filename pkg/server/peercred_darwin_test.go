package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A peer that hung up before the server asked is refused: macOS has no PID
// for it (ENOTCONN), where Linux keeps the one recorded at connect.
func TestPeerCredRefusesAPeerThatHungUp(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "bc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "c.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_ = c.Close()
	if _, _, err := peerCred(s); err == nil {
		t.Error("got nil, want an error for a peer that hung up")
	}
}
