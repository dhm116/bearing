//go:build linux || darwin

package server

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProcStatusReadsThisProcessesOwnerAndParent(t *testing.T) {
	t.Parallel()
	uid, ppid, err := procStatus(os.Getpid())
	if err != nil || uid != os.Getuid() || ppid != os.Getppid() {
		t.Errorf("got %d, %d, %v, want %d, %d", uid, ppid, err, os.Getuid(), os.Getppid())
	}
}

func TestPeerCredRefusesAClosedConnection(t *testing.T) {
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
	_ = c.Close()
	if _, _, err := peerCred(c); err == nil {
		t.Error("got nil, want an error for a closed connection")
	}
}

func TestProcStatusSaysAnExitedProcessDoesNotExist(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := procStatus(cmd.Process.Pid); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v, want fs.ErrNotExist for an exited process", err)
	}
}

// childSocketEnv tells a re-run of the test binary to be the child: connect
// to the socket it names and exit 0 if the server closes the connection.
const childSocketEnv = "BEARING_TEST_CHILD_SOCKET"

// A process this server started connects to the real socket and is closed;
// the test process itself, not a descendant of itself, is served (C-API-8).
func TestPeerListenerRefusesARealChildOfTheServer(t *testing.T) {
	if sock := os.Getenv(childSocketEnv); sock != "" {
		c, err := net.Dial("unix", sock) //nolint:gosec // G704: the test's own socket, named by its parent
		if err != nil {
			os.Exit(2)
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
			os.Exit(1) // served, not closed
		}
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	pl := &peerListener{Listener: ln, uid: os.Getuid()}
	t.Cleanup(func() { _ = pl.Close() })
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()

	cmd := exec.Command(os.Args[0], "-test.run=^TestPeerListenerRefusesARealChildOfTheServer$") //nolint:gosec // G204: re-runs this test binary as the child
	cmd.Env = append(os.Environ(), childSocketEnv+"="+sock)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("got child %v (%s), want its connection closed", err, out)
	}
	select {
	case <-accepted:
		t.Fatal("got the child's connection accepted, want it refused")
	default:
	}

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	select {
	case got := <-accepted:
		_ = got.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("got this process refused, want it served")
	}
}
