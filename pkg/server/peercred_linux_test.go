package server

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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

func TestUndumpableClearsTheDumpableFlag(t *testing.T) {
	t.Parallel()
	if err := Undumpable(); err != nil {
		t.Fatal(err)
	}
	if got, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0); errno != 0 || got != 0 {
		t.Errorf("got dumpable %d (%v), want 0", got, errno)
	}
}

// childSocketEnv tells a re-run of the test binary to be the child: connect
// to the socket it names and exit 0 if the server closes the connection.
const childSocketEnv = "BEARING_TEST_CHILD_SOCKET"

// A process this server started connects to the real socket and is closed;
// the test process itself, not a descendant of itself, is served (C-API-8).
func TestPeerListenerRefusesARealChildOfTheServer(t *testing.T) {
	if sock := os.Getenv(childSocketEnv); sock != "" {
		c, err := net.Dial("unix", sock)
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

	cmd := exec.Command(os.Args[0], "-test.run=^TestPeerListenerRefusesARealChildOfTheServer$")
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
