package testkit

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// Compile-time checks that the fakes satisfy their interfaces.
var (
	_ Clock          = (*FakeClock)(nil)
	_ IDs            = (*SeqIDs)(nil)
	_ SecretResolver = (*Secrets)(nil)
)

func recv(t *testing.T, ch <-chan time.Time) (time.Time, bool) {
	t.Helper()
	select {
	case v := <-ch:
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestClockNowAdvanceSet(t *testing.T) {
	c := NewClock(t0)
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("Now = %v, want %v", got, t0)
	}
	c.Advance(90 * time.Second)
	if got, want := c.Now(), t0.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("after Advance, Now = %v, want %v", got, want)
	}
	c.Set(t0)
	if got := c.Now(); !got.Equal(t0) {
		t.Fatalf("after Set, Now = %v, want %v", got, t0)
	}
}

func TestTimer(t *testing.T) {
	tests := []struct {
		name      string
		d         time.Duration
		advance   []time.Duration
		wantFired bool
		wantAt    time.Time
	}{
		{"before deadline", time.Minute, []time.Duration{59 * time.Second}, false, time.Time{}},
		{"at deadline", time.Minute, []time.Duration{time.Minute}, true, t0.Add(time.Minute)},
		{"past deadline in steps", time.Minute, []time.Duration{30 * time.Second, 45 * time.Second}, true, t0.Add(time.Minute)},
		{"zero fires immediately", 0, nil, true, t0},
		{"negative fires immediately", -time.Second, nil, true, t0.Add(-time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClock(t0)
			tm := c.NewTimer(tt.d)
			for _, d := range tt.advance {
				c.Advance(d)
			}
			got, fired := recv(t, tm.C)
			if fired != tt.wantFired || !got.Equal(tt.wantAt) {
				t.Fatalf("fired=%v at %v, want fired=%v at %v", fired, got, tt.wantFired, tt.wantAt)
			}
			if c.Waiters() != 0 && fired {
				t.Fatalf("fired timer still pending")
			}
		})
	}
}

func TestTimerStopAndReset(t *testing.T) {
	c := NewClock(t0)
	tm := c.NewTimer(time.Minute)
	if !tm.Stop() {
		t.Fatal("Stop on pending timer = false")
	}
	if tm.Stop() {
		t.Fatal("second Stop = true")
	}
	c.Advance(time.Hour)
	if _, fired := recv(t, tm.C); fired {
		t.Fatal("stopped timer fired")
	}

	if tm.Reset(time.Minute) {
		t.Fatal("Reset of stopped timer reported active")
	}
	if !tm.Reset(2 * time.Minute) {
		t.Fatal("Reset of pending timer reported inactive")
	}
	c.Advance(time.Minute)
	if _, fired := recv(t, tm.C); fired {
		t.Fatal("timer fired at old deadline")
	}
	c.Advance(time.Minute)
	if got, fired := recv(t, tm.C); !fired || !got.Equal(t0.Add(time.Hour+2*time.Minute)) {
		t.Fatalf("got %v, %v", got, fired)
	}

	// A fired but unread value is drained by Stop and Reset.
	tm.Reset(time.Second)
	c.Advance(time.Second)
	tm.Reset(time.Second)
	if _, fired := recv(t, tm.C); fired {
		t.Fatal("Reset left a stale value in C")
	}
}

func TestAfter(t *testing.T) {
	c := NewClock(t0)
	ch := c.After(time.Second)
	c.Advance(time.Second)
	if _, fired := recv(t, ch); !fired {
		t.Fatal("After did not fire")
	}
}

func TestSetFiresEveryDueTimer(t *testing.T) {
	c := NewClock(t0)
	var got []int
	chans := []<-chan time.Time{
		c.After(3 * time.Second),
		c.After(1 * time.Second),
		c.After(2 * time.Second),
	}
	late := c.After(time.Hour)
	c.Set(t0.Add(5 * time.Second))
	for i, ch := range chans {
		v, ok := recv(t, ch)
		if !ok {
			t.Fatalf("timer %d did not fire", i)
		}
		got = append(got, int(v.Sub(t0)/time.Second))
	}
	if want := []int{3, 1, 2}; !slices.Equal(got, want) {
		t.Fatalf("deadlines %v, want %v", got, want)
	}
	if _, ok := recv(t, late); ok || c.Waiters() != 1 {
		t.Fatalf("later timer fired or was dropped; waiters = %d", c.Waiters())
	}
}

func TestTicker(t *testing.T) {
	c := NewClock(t0)
	tk := c.NewTicker(10 * time.Second)
	c.Advance(9 * time.Second)
	if _, ok := recv(t, tk.C); ok {
		t.Fatal("ticked early")
	}
	c.Advance(time.Second)
	if got, ok := recv(t, tk.C); !ok || !got.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("first tick %v, %v", got, ok)
	}
	// A long jump delivers one tick and drops the rest, like time.Ticker.
	c.Advance(time.Minute)
	if got, ok := recv(t, tk.C); !ok || !got.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("tick after jump %v, %v", got, ok)
	}
	if _, ok := recv(t, tk.C); ok {
		t.Fatal("more than one tick buffered")
	}
	// The next deadline stays on the original grid.
	c.Advance(10 * time.Second) // now t0+80s
	if got, ok := recv(t, tk.C); !ok || !got.Equal(t0.Add(80*time.Second)) {
		t.Fatalf("tick on grid %v, %v", got, ok)
	}

	tk.Reset(time.Second)
	c.Advance(time.Second)
	if got, ok := recv(t, tk.C); !ok || !got.Equal(t0.Add(81*time.Second)) {
		t.Fatalf("tick after Reset %v, %v", got, ok)
	}
	tk.Stop()
	c.Advance(time.Hour)
	if _, ok := recv(t, tk.C); ok {
		t.Fatal("stopped ticker ticked")
	}
	if n := c.Waiters(); n != 0 {
		t.Fatalf("Waiters = %d after Stop", n)
	}
}

func TestTickerPanicsOnNonPositive(t *testing.T) {
	c := NewClock(t0)
	for name, f := range map[string]func(){
		"NewTicker": func() { c.NewTicker(0) },
		"Reset":     func() { c.NewTicker(time.Second).Reset(-1) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			f()
		})
	}
}

func TestBlockUntilWaiters(t *testing.T) {
	c := NewClock(t0)
	done := make(chan time.Time)
	go func() {
		done <- <-c.After(time.Minute)
	}()
	if err := c.BlockUntilWaiters(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if got := <-done; !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("got %v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.BlockUntilWaiters(ctx, 1); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestClockConcurrent drives a ticker-based loop, like the scheduler will,
// while other goroutines read the clock and create and stop timers. Run with
// -race.
func TestClockConcurrent(t *testing.T) {
	c := NewClock(t0)
	tk := c.NewTicker(time.Second)
	defer tk.Stop()

	var wg sync.WaitGroup
	ticks := make(chan time.Time, 100)
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case v := <-tk.C:
				ticks <- v
			case <-stop:
				return
			}
		}
	}()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = c.Now()
				tm := c.NewTimer(time.Millisecond)
				tm.Reset(2 * time.Millisecond)
				tm.Stop()
				_ = c.Waiters()
			}
		}()
	}
	for i := range 50 {
		// Wait for the previous tick to be consumed so none are dropped.
		if i > 0 {
			<-ticks
		}
		c.Advance(time.Second)
	}
	<-ticks
	close(stop)
	wg.Wait()
	if got, want := c.Now(), t0.Add(50*time.Second); !got.Equal(want) {
		t.Fatalf("Now = %v, want %v", got, want)
	}
}
