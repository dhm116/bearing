package server

import (
	"sync"
	"time"
)

// bucket is a token bucket: it holds up to burst tokens and refills at rate
// tokens a second.
type bucket struct {
	tokens float64
	at     time.Time
}

// limiter rate-limits by key with token buckets. A zero perMinute turns it
// off. The table is bounded: at maxKeys it drops the keys that have refilled
// completely, then, if the table is still full, the whole table, which lets
// a flood from many peers reset its own limits but never grows memory.
type limiter struct {
	rate  float64 // tokens a second
	burst float64
	max   int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

func newLimiter(perMinute, burst, maxKeys int, now func() time.Time) *limiter {
	return &limiter{rate: float64(perMinute) / 60, burst: float64(burst), max: maxKeys, now: now, buckets: map[string]*bucket{}}
}

// allow takes a token for key and reports whether there was one.
func (l *limiter) allow(key string) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max {
			l.sweep(now)
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops the buckets that have refilled, and everything if that was not
// enough.
func (l *limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.max {
		clear(l.buckets)
	}
}
