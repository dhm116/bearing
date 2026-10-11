//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package audit

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive (appending) or shared (reading) flock on file
// and waits for it. The lock goes when file is closed. Holders keep it for one
// write and sync, so the wait is short.
func lockFile(file *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err := syscall.Flock(int(file.Fd()), how) //nolint:gosec // G115: a file descriptor fits an int
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
