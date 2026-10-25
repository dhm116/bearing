//go:build !unix

package boot

import (
	"errors"
	"os"
)

// checkOwner refuses every file here, so the server does not start: with no
// way to tell who owns the settings or an adapter's executable, nothing
// vouches that only the service user can change them.
func checkOwner(os.FileInfo) error {
	return errors.New("this platform is unsupported: the server needs Unix file ownership to vouch for its settings and adapters")
}
