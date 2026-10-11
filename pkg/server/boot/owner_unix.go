//go:build unix

package boot

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkOwner refuses a file owned by anyone but the service user or root: the
// owner can swap its contents for code that runs as the service.
func checkOwner(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot tell who owns it on this platform")
	}
	if uid := int(st.Uid); uid != os.Geteuid() && uid != 0 {
		return fmt.Errorf("must be owned by the service user or root, not uid %d", uid)
	}
	return nil
}
