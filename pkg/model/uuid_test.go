package model

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestUUIDv7IsMonotonic(t *testing.T) {
	at := time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC)
	ids := NewUUIDv7Source(func() time.Time { return at }, bytes.NewReader(bytes.Repeat([]byte{0xff}, 64)))
	first := ids.NewID()
	if first != "01a0e5a2-1d90-7fff-bfff-ffffffffffff" {
		t.Fatalf("got %s, want the all-ones random bits at the mint time", first)
	}
	// Within the millisecond the random bits overflow into the timestamp.
	second := ids.NewID()
	if second != "01a0e5a2-1d91-7000-8000-000000000000" || second <= first {
		t.Fatalf("got %s, want the next ID after %s", second, first)
	}
	at = at.Add(-time.Hour) // a clock step back keeps the order
	if third := ids.NewID(); third <= second || !strings.HasPrefix(third, "01a0e5a2-1d91-7000-8000-") || ids.Last() != third {
		t.Fatalf("got %s (last %s), want an ID after %s", third, ids.Last(), second)
	}
}

func TestUUIDv7PanicsWithoutRandomness(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("got no panic, want one")
		}
	}()
	NewUUIDv7Source(func() time.Time { return time.Unix(1, 0) }, bytes.NewReader(nil)).NewID()
}

func TestUUIDv7SeedMovesPastAnIDIssuedElsewhere(t *testing.T) {
	at := time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC)
	ids := NewUUIDv7Source(func() time.Time { return at }, bytes.NewReader(make([]byte, 64)))
	// An ID from a clock about an hour ahead, as a restored backup may hold.
	ahead := "01a0e5de-b890-7abc-8000-0000000000ff"
	if err := ids.Seed(ahead); err != nil {
		t.Fatal(err)
	}
	if got := ids.NewID(); got != "01a0e5de-b890-7abc-8000-000000000100" {
		t.Fatalf("got %s, want the ID after %s", got, ahead)
	}
	// Seeding an older ID changes nothing.
	if err := ids.Seed("01a0e5a2-1d90-7000-8000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if got := ids.NewID(); got != "01a0e5de-b890-7abc-8000-000000000101" {
		t.Fatalf("got %s, want the order kept after an older seed", got)
	}
	if err := ids.Seed("not-a-uuid"); err == nil {
		t.Fatal("got no error, want one for a seed that isn't a UUIDv7")
	}
}

func TestUUIDv7Time(t *testing.T) {
	at := time.Date(2026, 9, 28, 1, 30, 2, 345_000_000, time.UTC)
	id := NewUUIDv7Source(func() time.Time { return at }, bytes.NewReader(make([]byte, 64))).NewID()
	if got, err := UUIDv7Time(id); err != nil || !got.Equal(at) {
		t.Fatalf("got %s, %v, want %s", got, err, at)
	}
	if _, err := UUIDv7Time("00000000-0000-4000-8000-000000000001"); err == nil {
		t.Fatal("got no error, want one for a version 4 UUID")
	}
}
