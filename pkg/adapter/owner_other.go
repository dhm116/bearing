//go:build !unix

package adapter

import "os"

const openFlags = os.O_RDONLY

// checkOwner checks nothing: there is no Unix owner to check here.
func checkOwner(os.FileInfo) error { return nil }
