// Package clock is the time source Bearing components depend on instead of
// calling package time directly, so tests can drive time by hand. [Real]
// wraps package time; tests use the fake in internal/testkit.
package clock

import "time"

// Clock tells the time and creates timers and tickers.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has elapsed.
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
	// NewTicker panics if d <= 0, like time.NewTicker.
	NewTicker(d time.Duration) Ticker
}

// Timer is a one-shot timer with the semantics of time.Timer.
type Timer interface {
	// C returns the channel the timer fires on.
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker delivers ticks at a fixed period with the semantics of time.Ticker.
type Ticker interface {
	// C returns the channel ticks are delivered on.
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// Real is the [Clock] backed by package time.
type Real struct{}

var _ Clock = Real{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTimer wraps time.NewTimer.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

// NewTicker wraps time.NewTicker.
func (Real) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time   { return r.t.C }
func (r realTicker) Stop()                 { r.t.Stop() }
func (r realTicker) Reset(d time.Duration) { r.t.Reset(d) }
