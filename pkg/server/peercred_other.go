//go:build !linux && !darwin

package server

import (
	"errors"
	"net"
)

// peerCred has no implementation off Linux and macOS, so the local socket refuses every
// connection there: the server fails closed rather than trust a peer it cannot
// identify. Use the TCP listener with the API token.
func peerCred(net.Conn) (uid, pid int, err error) {
	return 0, 0, errors.New("peer credentials are not supported on this system")
}

// procStatus is never reached here: peerCred refuses first.
func procStatus(int) (uid, ppid int, err error) {
	return 0, 0, errors.New("process status is not supported on this system")
}

// Undumpable does nothing here.
func Undumpable() error { return nil }
