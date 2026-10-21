// Package conformance holds test suites that every implementation of a
// contracts interface must pass. A backend's own tests call the suite with a
// factory that returns a fresh, empty store.
package conformance

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
