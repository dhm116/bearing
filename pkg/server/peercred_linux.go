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

// procStatus returns the real UID and the parent PID of pid from
// /proc/<pid>/status. The status file names the real owner even of a
// non-dumpable process, whose /proc entries the kernel gives to root.
func procStatus(pid int) (uid, ppid int, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0, err
	}
	uid, ppid = -1, -1
	for line := range strings.Lines(string(b)) {
		k, v, _ := strings.Cut(line, ":")
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "Uid":
			uid, err = strconv.Atoi(f[0])
		case "PPid":
			ppid, err = strconv.Atoi(f[0])
		}
		if err != nil {
			return 0, 0, fmt.Errorf("process %d: malformed status: %w", pid, err)
		}
	}
	if uid < 0 || ppid < 0 {
		return 0, 0, fmt.Errorf("process %d: malformed status", pid)
	}
	return uid, ppid, nil
}

// Undumpable marks this process non-dumpable, so a process of the same user,
// such as a local adapter, can neither ptrace it nor read its
// /proc/<pid>/environ or mem, where the server's secrets are (C-SECRET-3).
// The adapters it starts are dumpable again: exec resets the flag.
func Undumpable() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return fmt.Errorf("server: mark the process non-dumpable: %w", errno)
	}
	return nil
}
