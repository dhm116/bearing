//go:build !linux

package server

import (
	"errors"
	"net"
)

// peerUID has no implementation off Linux, so the local socket refuses every
// connection there: the server fails closed rather than trust a peer it cannot
// identify. Use the TCP listener with the API token.
func peerUID(net.Conn) (int, error) {
	return 0, errors.New("peer credentials are not supported on this system")
}
