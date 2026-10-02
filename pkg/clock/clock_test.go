package clock

import (
	"testing"
	"time"
)

func TestRealNow(t *testing.T) {
	before := time.Now()
	got := Real{}.Now()
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("Now = %v, outside [%v, now]", got, before)
	}
}

func TestRealAfter(t *testing.T) {
	select {
	case <-Real{}.After(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Fatal("After did not fire")
	}
}

func TestRealTimer(t *testing.T) {
	var c Clock = Real{}
	tm := c.NewTimer(time.Hour)
	if !tm.Stop() {
		t.Fatal("Stop on pending timer = false")
	}
	if tm.Reset(time.Millisecond) {
		t.Fatal("Reset on stopped timer = true")
	}
	select {
	case <-tm.C():
	case <-time.After(5 * time.Second):
		t.Fatal("timer did not fire")
	}
}

func TestRealTicker(t *testing.T) {
	tk := Real{}.NewTicker(time.Hour)
	defer tk.Stop()
	tk.Reset(time.Millisecond)
	for range 2 {
		select {
		case <-tk.C():
		case <-time.After(5 * time.Second):
			t.Fatal("ticker did not tick")
		}
	}
}
