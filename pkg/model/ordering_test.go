package model

import (
	"testing"
	"time"

	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
)

func TestCompareOrderingKeysGoesLeftToRight(t *testing.T) {
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	key := func(at time.Time, obs, ev, hash string) *resolverv1alpha1.OrderingKey {
		return NewOrderingKey(at, obs, ev, hash)
	}
	base := key(at, "obs-b", "src/ev-b", "hash-b")
	tests := []struct {
		name string
		a, b *resolverv1alpha1.OrderingKey
		want int
	}{
		{"equal", base, key(at, "obs-b", "src/ev-b", "hash-b"), 0},
		{"observed_at beats everything after it", key(at.Add(time.Microsecond), "obs-a", "src/ev-a", "hash-a"), base, 1},
		{"a later field never overrides an earlier one", base, key(at.Add(time.Microsecond), "obs-a", "src/ev-a", "hash-a"), -1},
		{"observation_id", key(at, "obs-c", "src/ev-a", "hash-a"), base, 1},
		{"event_id", key(at, "obs-b", "src/ev-a", "hash-z"), base, -1},
		{"content_hash", key(at, "obs-b", "src/ev-b", "hash-c"), base, 1},
		{"nanoseconds are below the precision", key(at.Add(999*time.Nanosecond), "obs-b", "src/ev-b", "hash-b"), base, 0},
		{"UTF-8 bytes, not code points", key(at, "\U0001F600", "", ""), key(at, "￿", "", ""), 1},
		{"nil sorts first", nil, base, -1},
		{"nil against nil", nil, nil, 0},
		{"no observed_at sorts first", &resolverv1alpha1.OrderingKey{EventId: "z"}, base, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompareOrderingKeys(tt.a, tt.b); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
			if got := CompareOrderingKeys(tt.b, tt.a); got != -tt.want {
				t.Fatalf("reversed: got %d, want %d", got, -tt.want)
			}
		})
	}
}
