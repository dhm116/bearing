package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLimiterEvictsTheKeyLeastRecentlySeenAndNeverRefusesANewOne(t *testing.T) {
	now := epoch
	l := newLimiter(1, 1, 3, func() time.Time { return now })
	for _, k := range []string{"a", "b", "c"} {
		if !l.allow(k) {
			t.Fatalf("first request for %s was refused", k)
		}
	}
	// a is out of tokens and keeps asking, so it is the most recently seen.
	if l.allow("a") {
		t.Fatal("a second request for a was allowed")
	}
	// The table is full; d takes the place of b, the key seen least recently.
	if !l.allow("d") {
		t.Fatal("a new key was refused")
	}
	if n := len(l.buckets); n != 3 || l.seen.Len() != 3 {
		t.Fatalf("got %d buckets and %d in the order, want 3 and 3", n, l.seen.Len())
	}
	if _, kept := l.buckets["b"]; kept {
		t.Error("b, the least recently seen, was kept")
	}
	if l.allow("a") {
		t.Error("a was let off its debt when others came and went")
	}
	// A flood of new keys pushes the idle ones out and never gets a refusal.
	for i := range 100 {
		if !l.allow(strconv.Itoa(i)) {
			t.Fatalf("new key %d was refused", i)
		}
	}
	if n := len(l.buckets); n != 3 {
		t.Fatalf("got %d buckets, want the table held at 3", n)
	}
}

func TestAddrKeyGroupsAnIPv6PeerByItsSlash64(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		a, b string
		same bool
	}{
		{"192.0.2.1", "192.0.2.1", true},
		{"192.0.2.1", "192.0.2.2", false},
		{"192.0.2.1", "::ffff:192.0.2.1", true},
		{"2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:ffff", true},
		{"2001:db8:1:2::1", "2001:db8:1:3::1", false},
		{"fe80::1%eth0", "fe80::1%eth1", true},
		{"not an address", "not an address", true},
		{"not an address", "another", false},
	} {
		t.Run(tt.a+" "+tt.b, func(t *testing.T) {
			if got := addrKey(tt.a) == addrKey(tt.b); got != tt.same {
				t.Fatalf("got keys %q and %q, same is %v, want %v", addrKey(tt.a), addrKey(tt.b), got, tt.same)
			}
		})
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

// endlessBody is a body that never ends and counts what was read.
type endlessBody struct{ read *int64 }

func (e endlessBody) Read(p []byte) (int, error) {
	// Fail loudly after 16 MiB rather than fill the test's memory when the
	// read is not bounded.
	if *e.read > 16<<20 {
		return 0, errors.New("read far past the limit")
	}
	*e.read += int64(len(p))
	return len(p), nil
}

func TestReadBodyStopsReadingOnceTheLimitIsPassed(t *testing.T) {
	var read int64
	r := httptest.NewRequest(http.MethodPost, "/", endlessBody{&read})
	r.ContentLength = -1
	if _, err := readBody(r, 1000); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("got %v after %d bytes, want errBodyTooLarge", err, read)
	}
	// io.ReadAll reads in growing chunks, so allow for one past the limit.
	if read > 4096 {
		t.Fatalf("read %d bytes of an endless body, want about the limit of 1000", read)
	}
}
