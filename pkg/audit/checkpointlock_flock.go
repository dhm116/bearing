//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockPoll is how often a lock that is held is tried again.
const lockPoll = 10 * time.Millisecond

// lockFile takes an exclusive (appending) or shared (reading) flock on file.
// It waits for a lock that another holder has, until ctx is done or
// MaxLockWait passes: holders keep it for one write and sync, so a longer wait
// is a stopped process or a hung file system, and the caller should hear of
// it rather than stall. The lock goes when file is closed.
func lockFile(ctx context.Context, file *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	deadline := time.Now().Add(MaxLockWait)
	for {
		err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another process has held a lock on %s for over %s", file.Name(), MaxLockWait)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for a lock on %s: %w", file.Name(), ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// lockUnsupported reports whether err says the file system has no file locks
// (some network, FUSE and read-only archive mounts).
func lockUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}
