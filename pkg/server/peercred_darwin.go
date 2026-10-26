package server

import (
	"errors"
	"fmt"
	"io/fs"
	"net"

	"golang.org/x/sys/unix"
)

// peerCred returns the UID and PID of the process at the other end of a Unix
// socket connection, from the kernel (LOCAL_PEERCRED and LOCAL_PEERPID), not
// from anything the peer said. The PID is the process that connected.
func peerCred(c net.Conn) (uid, pid int, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("not a Unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *unix.Xucred
	var credErr, pidErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, 0, err
	}
	if err := errors.Join(credErr, pidErr); err != nil {
		return 0, 0, err
	}
	return int(cred.Uid), pid, nil
}

// procStatus returns the real UID and the parent PID of pid from the kernel's
// process table (sysctl kern.proc.pid), as /proc/<pid>/status does on Linux.
// A PID with no process gets an empty answer, which x/sys reports as EIO;
// that is fs.ErrNotExist here, so descendant treats it as an exited ancestor.
func procStatus(pid int) (uid, ppid int, err error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if errors.Is(err, unix.EIO) {
		return 0, 0, fmt.Errorf("process %d: %w", pid, fs.ErrNotExist)
	}
	if err != nil {
		return 0, 0, err
	}
	return int(kp.Eproc.Pcred.P_ruid), int(kp.Eproc.Ppid), nil
}

// Undumpable does nothing on macOS, which has no dumpable flag: a process of
// the same user, such as a local adapter, can read the server's environment
// (C-SECRET-3 holds on Linux only).
func Undumpable() error { return nil }
