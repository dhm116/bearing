//go:build !linux

package server

import (
	"errors"
	"net"
)

// peerCred has no implementation off Linux, so the local socket refuses every
// connection there: the server fails closed rather than trust a peer it cannot
// identify. Use the TCP listener with the API token.
func peerCred(net.Conn) (uid, pid int, err error) {
	return 0, 0, errors.New("peer credentials are not supported on this system")
}

// procParent is never reached off Linux: peerCred refuses first.
func procParent(int) (int, error) {
	return 0, errors.New("process parents are not supported on this system")
}
