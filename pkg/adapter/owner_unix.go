//go:build unix

package adapter

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const openFlags = os.O_RDONLY | syscall.O_NONBLOCK

// checkOwner refuses a file owned by anyone but this process's user or root.
func checkOwner(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("has no owner this platform can tell")
	}
	if !ownerOK(st.Uid, os.Geteuid()) {
		return fmt.Errorf("must be owned by this process's user or root, not uid %d", st.Uid)
	}
	return nil
}
