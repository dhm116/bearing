package testkit

import (
	"regexp"
	"sync"
	"testing"
	"time"
)

func TestSeqIDs(t *testing.T) {
	tests := []struct {
		name string
		ids  *SeqIDs
		want []string
	}{
		{"prefix", NewSeqIDs("evt-"), []string{"evt-1", "evt-2", "evt-3"}},
		{"no prefix", NewSeqIDs(""), []string{"1", "2", "3"}},
		{"uuid", NewUUIDs(), []string{
			"00000000-0000-4000-8000-000000000001",
			"00000000-0000-4000-8000-000000000002",
			"00000000-0000-4000-8000-000000000003",
		}},
	}
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i, want := range tt.want {
				got := tt.ids.NewID()
				if got != want {
					t.Fatalf("call %d: got %q, want %q", i+1, got, want)
				}
				if tt.ids.uuid && !uuidV4.MatchString(got) {
					t.Fatalf("%q is not a version 4 UUID", got)
				}
			}
		})
	}
}

func TestSeqIDsConcurrent(t *testing.T) {
	ids := NewSeqIDs("s")
	const workers, each = 8, 250
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
		wg   sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				id := ids.NewID()
				mu.Lock()
				if seen[id] {
					t.Errorf("duplicate id %s", id)
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != workers*each {
		t.Fatalf("got %d distinct ids, want %d", len(seen), workers*each)
	}
}

func TestUUIDv7sAreDeterministic(t *testing.T) {
	clk := NewClock(time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC))
	ids := NewUUIDv7s(clk.Now)
	for i, want := range []string{"01a0e5a2-1d90-7000-8000-000000000000", "01a0e5a2-1d90-7000-8000-000000000001"} {
		if got := ids.NewID(); got != want {
			t.Fatalf("call %d: got %q, want %q", i+1, got, want)
		}
	}
	clk.Advance(time.Millisecond)
	if got := ids.NewID(); got != "01a0e5a2-1d91-7000-8000-000000000000" {
		t.Fatalf("got %q, want fresh zero bits in the next millisecond", got)
	}
}
