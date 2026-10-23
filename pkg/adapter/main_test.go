package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenRefusesBadAddressesAndMakesAPrivateSocket(t *testing.T) {
	for _, addr := range []string{"", "unix://rel.sock", "http://x:1", "::"} {
		if _, err := Listen(addr, "tok"); err == nil {
			t.Errorf("%q: got a listener, want a refusal", addr)
		}
	}
	dir, err := os.MkdirTemp("", "ad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.sock")
	lis, err := Listen("unix://"+path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("got %v, %v, want a socket with mode 0600", fi.Mode(), err)
	}
}

func TestListenRefusesATCPAddressWithoutAToken(t *testing.T) {
	if _, err := Listen("tcp://127.0.0.1:0", ""); err == nil || !strings.Contains(err.Error(), EnvToken) {
		t.Fatalf("got %v, want a refusal naming %s", err, EnvToken)
	}
	lis, err := Listen("tcp://127.0.0.1:0", "tok")
	if err != nil {
		t.Fatalf("got %v, want a listener with a token", err)
	}
	_ = lis.Close()
}
