package testkit

import (
	"context"
	"sync"
	"time"
)

// Clock is a source of the current time. time.Now satisfies it as a
// function; [FakeClock] satisfies it as a value.
type Clock interface {
	Now() time.Time
}

// FakeClock is a [Clock] that only moves when a test calls [FakeClock.Set]
// or [FakeClock.Advance]. Timers and tickers created from it fire when the
// clock reaches their deadline. It is safe for concurrent use.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	changed chan struct{} // closed and replaced whenever waiters change
}

// waiter is a pending timer (period 0) or ticker deadline.
type waiter struct {
	when   time.Time
	period time.Duration
	ch     chan time.Time
}

// NewClock returns a FakeClock set to start.
func NewClock(start time.Time) *FakeClock {
	return &FakeClock{now: start, changed: make(chan struct{})}
}

// Now returns the clock's current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d and fires every timer and ticker
// whose deadline has been reached, earliest first.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(c.now.Add(d))
}

// Set moves the clock to t and fires every timer and ticker whose deadline
// is at or before t, earliest first. Setting an earlier time fires nothing.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(t)
}

func (c *FakeClock) setLocked(t time.Time) {
	c.now = t
	fired := false
	for {
		next := -1
		for i, w := range c.waiters {
			if !w.when.After(t) && (next < 0 || w.when.Before(c.waiters[next].when)) {
				next = i
			}
		}
		if next < 0 {
			break
		}
		w := c.waiters[next]
		send(w.ch, w.when)
		if w.period > 0 {
			// Like time.Ticker, ticks the receiver is too slow for are dropped.
			w.when = w.when.Add(w.period * (t.Sub(w.when)/w.period + 1))
			continue
		}
		c.removeLocked(w)
		fired = true
	}
	if fired {
		c.notifyLocked()
	}
}

// After returns a channel that receives the deadline once the clock has
// advanced by d.
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	return c.NewTimer(d).C
}

// Timer is a one-shot timer driven by a [FakeClock].
type Timer struct {
	// C receives the deadline when the timer fires.
	C <-chan time.Time
	c *FakeClock
	w *waiter
}

// NewTimer returns a timer that fires once the clock has advanced by d. A
// timer with d <= 0 fires immediately.
func (c *FakeClock) NewTimer(d time.Duration) *Timer {
	w := &waiter{ch: make(chan time.Time, 1)}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLocked(w, d)
	return &Timer{C: w.ch, c: c, w: w}
}

// Stop prevents the timer from firing and drains any undelivered value from
// C, matching time.Timer since Go 1.23. It reports whether the timer was
// pending.
func (t *Timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	return t.c.stopLocked(t.w)
}

// Reset stops the timer and restarts it to fire after d. It reports whether
// the timer was pending.
func (t *Timer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	active := t.c.stopLocked(t.w)
	t.c.startLocked(t.w, d)
	return active
}

// Ticker delivers ticks at a fixed period of [FakeClock] time.
type Ticker struct {
	// C receives the deadline of each tick. Like time.Ticker it buffers one
	// tick and drops ticks the receiver is too slow for.
	C <-chan time.Time
	c *FakeClock
	w *waiter
}

// NewTicker returns a ticker with period d. It panics if d <= 0, like
// time.NewTicker.
func (c *FakeClock) NewTicker(d time.Duration) *Ticker {
	if d <= 0 {
		panic("testkit: non-positive interval for NewTicker")
	}
	w := &waiter{ch: make(chan time.Time, 1), period: d}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLocked(w, d)
	return &Ticker{C: w.ch, c: c, w: w}
}

// Stop turns off the ticker and drains any undelivered tick.
func (t *Ticker) Stop() {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.c.stopLocked(t.w)
}

// Reset stops the ticker and restarts it with period d, the first tick d
// after the current time. It panics if d <= 0.
func (t *Ticker) Reset(d time.Duration) {
	if d <= 0 {
		panic("testkit: non-positive interval for Ticker.Reset")
	}
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.c.stopLocked(t.w)
	t.w.period = d
	t.c.startLocked(t.w, d)
}

// Waiters returns the number of pending timers and tickers.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// BlockUntilWaiters waits until at least n timers and tickers are pending,
// so a test can advance the clock only after the code under test has
// started waiting on it. It returns ctx.Err() if ctx ends first.
func (c *FakeClock) BlockUntilWaiters(ctx context.Context, n int) error {
	for {
		c.mu.Lock()
		if len(c.waiters) >= n {
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// startLocked schedules w to fire d from now, or fires it at once if d <= 0.
func (c *FakeClock) startLocked(w *waiter, d time.Duration) {
	w.when = c.now.Add(d)
	if d <= 0 {
		send(w.ch, w.when)
		return
	}
	c.waiters = append(c.waiters, w)
	c.notifyLocked()
}

// stopLocked unschedules w, drains its channel and reports whether it was
// pending.
func (c *FakeClock) stopLocked(w *waiter) bool {
	select {
	case <-w.ch:
	default:
	}
	if c.removeLocked(w) {
		c.notifyLocked()
		return true
	}
	return false
}

func (c *FakeClock) removeLocked(w *waiter) bool {
	for i, x := range c.waiters {
		if x == w {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return true
		}
	}
	return false
}

func (c *FakeClock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// send delivers t without blocking; a full channel drops it.
func send(ch chan time.Time, t time.Time) {
	select {
	case ch <- t:
	default:
	}
}
