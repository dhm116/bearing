//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package audit

import (
	"context"
	"errors"
	"os"
)

// lockFile has no file locks to take here. Reading goes on under the
// in-process lock; appending is refused, because another process could remove
// the line (see the flock version).
func lockFile(_ context.Context, _ *os.File, exclusive bool) error {
	if exclusive {
		return errors.New("audit: this platform has no file locks, so a checkpoint file cannot be appended to safely")
	}
	return nil
}

// lockUnsupported is false: there is nothing to try.
func lockUnsupported(error) bool { return false }
