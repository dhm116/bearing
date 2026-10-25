package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenRefusesBadAddressesAndMakesAPrivateSocket(t *testing.T) {
	for _, addr := range []string{"", "unix://rel.sock", "http://x:1", "::"} {
		if _, err := Listen(addr, func(string) string { return "" }); err == nil {
			t.Errorf("%q: got a listener, want a refusal", addr)
		}
	}
	dir, err := os.MkdirTemp("", "ad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "a.sock")
	lis, err := Listen("unix://"+path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("got %v, %v, want a socket with mode 0600", fi.Mode(), err)
	}
}
