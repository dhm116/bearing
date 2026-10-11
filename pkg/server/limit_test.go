package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterTableStaysBounded(t *testing.T) {
	now := epoch
	l := newLimiter(60, 1, 3, func() time.Time { return now })
	for _, k := range []string{"a", "b", "c"} {
		if !l.allow(k) {
			t.Fatalf("first request for %s was refused", k)
		}
	}
	// Nothing has refilled, so the full table is dropped to make room.
	if !l.allow("d") {
		t.Fatal("a new key was refused")
	}
	if n := len(l.buckets); n > 3 {
		t.Fatalf("got %d buckets, want at most 3", n)
	}
	// After a refill, only the idle keys are dropped; "d" keeps its debt.
	now = now.Add(time.Minute)
	l.allow("d")
	now = now.Add(100 * time.Millisecond)
	for _, k := range []string{"e", "f"} {
		l.allow(k)
	}
	if _, kept := l.buckets["d"]; !kept {
		t.Error("a key that had not refilled was dropped while others had")
	}
	if l.allow("d") {
		t.Error("the bucket of d was reset")
	}
}

func TestLimiterOffAllowsEverything(t *testing.T) {
	var l *limiter
	if !l.allow("x") {
		t.Fatal("a nil limiter refused")
	}
	if l := newLimiter(0, 0, 1, time.Now); !l.allow("x") {
		t.Fatal("a limiter with no rate refused")
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestReadBodyEnforcesTheLimitWhateverTheDeclaredLength(t *testing.T) {
	for _, tt := range []struct {
		name string
		body io.Reader
		len  int64
		want error
	}{
		{"within the limit", strings.NewReader("12345"), 5, nil},
		{"declared too long", strings.NewReader("1"), 6, errBodyTooLarge},
		{"longer than declared", strings.NewReader("123456"), 1, errBodyTooLarge},
		{"no declared length", strings.NewReader("123456"), -1, errBodyTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", tt.body)
			r.ContentLength = tt.len
			_, err := readBody(r, 5)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	r := httptest.NewRequest(http.MethodPost, "/", brokenBody{})
	if _, err := readBody(r, 5); err == nil || errors.Is(err, errBodyTooLarge) {
		t.Fatalf("got %v, want a read error", err)
	}
}

func TestPeerOfIgnoresForwardedHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", http.NoBody)
	r.RemoteAddr = "192.0.2.7:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("Forwarded", "for=203.0.113.9")
	if got := peerOf(r); got != "192.0.2.7" {
		t.Fatalf("got %q, want 192.0.2.7", got)
	}
	r.RemoteAddr = "@"
	if got := peerOf(r); got != "@" {
		t.Fatalf("got %q for an address with no port", got)
	}
}
