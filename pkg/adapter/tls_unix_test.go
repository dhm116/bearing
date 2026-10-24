//go:build unix

package adapter

import (
	"crypto/tls"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"bearing.example/internal/testkit"
)

// A FIFO swapped in for the key on reload is refused without waiting for a
// writer, so the handshake that reloads completes with the last good files.
func TestReloadDoesNotHangOnAFIFO(t *testing.T) {
	t.Parallel()
	f := testkit.NewTLSFiles(t)
	clk := testkit.NewClock(tlsTestStart)
	addr := serveTLS(t, tlsEnv(f), clk.Now)
	if err := os.Remove(f.ServerKey); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(f.ServerKey, 0o600); err != nil {
		t.Fatal(err)
	}
	clk.Advance(reloadInterval)
	pair, err := tls.LoadX509KeyPair(f.ClientCert, f.ClientKey)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: certPool(t, f.CA), Certificates: []tls.Certificate{pair},
	})
	if err != nil {
		t.Fatalf("handshake with a FIFO for the key: %v", err)
	}
	_ = conn.Close()
	if !strings.Contains(logs.all(), f.ServerKey+" is not a regular file") {
		t.Fatalf("got logs %q, want the FIFO refused", logs.all())
	}
}

// A file that changes owner to someone other than this user or root is
// refused on reload. Only root can give a file away, so this needs root.
func TestReloadRefusesAFileOwnedBySomeoneElse(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown a file")
	}
	t.Parallel()
	f := testkit.NewTLSFiles(t)
	clk := testkit.NewClock(tlsTestStart)
	addr := serveTLS(t, tlsEnv(f), clk.Now)
	first := presented(t, addr, f)
	f.Reissue(t)
	if err := os.Chown(f.CA, 1, -1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(reloadInterval)
	if got := presented(t, addr, f); got.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatalf("got serial %v, want the last good one, %v", got.SerialNumber, first.SerialNumber)
	}
	if !strings.Contains(logs.all(), "not uid 1") {
		t.Fatalf("got logs %q, want the owner refused", logs.all())
	}
}
