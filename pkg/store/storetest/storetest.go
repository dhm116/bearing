// Package storetest holds the conformance suites every store.Store
// implementation runs. A store's own tests call a suite with a factory that
// returns a fresh, empty store.
package storetest

import (
	"testing"
	"time"
)

// Clock is the store's clock as the suite drives it; testkit.FakeClock is
// one.
type Clock interface {
	Now() time.Time
	Set(t time.Time)
}

// ensure fails the test with the message unless ok.
func ensure(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
