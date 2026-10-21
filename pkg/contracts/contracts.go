// Package contracts defines the interfaces the server is built on that are
// not the graph (pkg/graph): the durable event log (EventLog) and who may
// call the API (Caller, Method). internal/duckstore implements EventLog and
// passes its conformance suite in pkg/contracts/conformance.
package contracts

import "errors"

// ErrNotFound is returned when a lookup matches nothing.
var ErrNotFound = errors.New("contracts: not found")
