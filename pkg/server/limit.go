package server

import (
	"container/list"
	"net"
	"strings"
	"sync"
	"time"
)

// bucket is a token bucket: it holds up to burst tokens and refills at rate
// tokens a second.
type bucket struct {
	key    string
	tokens float64
	at     time.Time
}

// limiter rate-limits by key with token buckets. A zero perMinute turns it
// off. The table is bounded: at maxKeys, a new key takes the place of the
// key seen least recently, so a flood of new keys pushes out the idle ones
// first and a key that keeps asking keeps its debt. It never refuses a new
// key for want of room (C-API-6).
type limiter struct {
	rate  float64 // tokens a second
	burst float64
	max   int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*list.Element // of *bucket
	seen    *list.List               // most recently seen first
}

func newLimiter(perMinute, burst, maxKeys int, now func() time.Time) *limiter {
	return &limiter{
		rate: float64(perMinute) / 60, burst: float64(burst), max: maxKeys, now: now,
		buckets: map[string]*list.Element{}, seen: list.New(),
	}
}

// entry is the bucket an element of the order holds.
func entry(el *list.Element) *bucket {
	b, _ := el.Value.(*bucket) // only buckets are ever pushed
	return b
}

// allow takes a token for key and reports whether there was one.
func (l *limiter) allow(key string) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var b *bucket
	if el, ok := l.buckets[key]; ok {
		l.seen.MoveToFront(el)
		b = entry(el)
	} else {
		if len(l.buckets) >= l.max {
			old := l.seen.Back()
			delete(l.buckets, entry(old).key)
			l.seen.Remove(old)
		}
		b = &bucket{key: key, tokens: l.burst, at: now}
		l.buckets[key] = l.seen.PushFront(b)
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// addrKey is the key a remote address is limited and counted under: the
// address itself for IPv4, and the /64 for IPv6, since one host owns a whole
// /64 and could otherwise take a fresh address for every request.
func addrKey(host string) string {
	host, _, _ = strings.Cut(host, "%") // a zone names the interface, not the peer
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if ip.To4() != nil {
		return ip.To4().String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
