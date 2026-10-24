package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// peerCred returns the UID and PID of the process at the other end of a Unix
// socket connection, from the kernel (SO_PEERCRED), not from anything the
// peer said. The PID is the process that connected.
func peerCred(c net.Conn) (uid, pid int, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("not a Unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *syscall.Ucred
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		cred, sockErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if sockErr != nil {
		return 0, 0, sockErr
	}
	return int(cred.Uid), int(cred.Pid), nil
}

// procParent returns the parent PID of pid from /proc/<pid>/stat, whose
// fourth field follows the command name in parentheses.
func procParent(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("process %d: malformed stat", pid)
	}
	return strconv.Atoi(f[1])
}
