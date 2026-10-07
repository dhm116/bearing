package model

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestUUIDv7IsMonotonic(t *testing.T) {
	at := time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC)
	newID := UUIDv7(func() time.Time { return at }, bytes.NewReader(bytes.Repeat([]byte{0xff}, 64)))
	first := newID()
	if first != "01a0e5a2-1d90-7fff-bfff-ffffffffffff" {
		t.Fatalf("got %s, want the all-ones random bits at the mint time", first)
	}
	// Within the millisecond the random bits overflow into the timestamp.
	second := newID()
	if second != "01a0e5a2-1d91-7000-8000-000000000000" || second <= first {
		t.Fatalf("got %s, want the next ID after %s", second, first)
	}
	at = at.Add(-time.Hour) // a clock step back keeps the order
	if third := newID(); third <= second || !strings.HasPrefix(third, "01a0e5a2-1d91-7000-8000-") {
		t.Fatalf("got %s, want an ID after %s", third, second)
	}
}

func TestUUIDv7PanicsWithoutRandomness(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("got no panic, want one")
		}
	}()
	UUIDv7(func() time.Time { return time.Unix(1, 0) }, bytes.NewReader(nil))()
}
