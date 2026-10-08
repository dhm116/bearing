package model

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// UUIDv7Source issues subject IDs: UUIDv7 text (RFC 9562) from a monotonic
// generator, so each ID is greater than the one before, even within a
// millisecond or if the clock steps back (RFC 9562 §6.2, method 2: the
// random bits of the last ID plus one). It is safe for concurrent use.
type UUIDv7Source struct {
	now func() time.Time
	rnd io.Reader

	mu     sync.Mutex
	lastMS uint64
	hi, lo uint64 // the 12 bits of rand_a and the 62 of rand_b
	last   string
}

// NewUUIDv7Source returns a source that reads the time from now and random
// bits from rnd. NewID panics if rnd fails.
func NewUUIDv7Source(now func() time.Time, rnd io.Reader) *UUIDv7Source {
	return &UUIDv7Source{now: now, rnd: rnd}
}

// NewID returns the next ID.
func (g *UUIDv7Source) NewID() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ms := uint64(g.now().UnixMilli())
	if ms > g.lastMS {
		var b [16]byte
		if _, err := io.ReadFull(g.rnd, b[:]); err != nil {
			panic("model: read random bits for a UUIDv7: " + err.Error())
		}
		g.lastMS, g.hi, g.lo = ms, binary.BigEndian.Uint64(b[:8])&0xfff, binary.BigEndian.Uint64(b[8:])&(1<<62-1)
	} else if g.lo++; g.lo == 1<<62 {
		if g.lo, g.hi = 0, g.hi+1; g.hi == 1<<12 {
			g.hi, g.lastMS = 0, g.lastMS+1
		}
	}
	var u [16]byte
	binary.BigEndian.PutUint64(u[:8], g.lastMS<<16|0x7000|g.hi)
	binary.BigEndian.PutUint64(u[8:], 1<<63|g.lo)
	s := hex.EncodeToString(u[:])
	g.last = s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
	return g.last
}

// Last returns the last ID issued or seeded, or "" if there is none.
func (g *UUIDv7Source) Last() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// Seed makes every later ID greater than last, an ID issued elsewhere, as a
// store does after a restore. Until the clock passes last's timestamp, IDs
// carry that timestamp, so they can drift from wall-clock time.
func (g *UUIDv7Source) Seed(last string) error {
	if !ValidSubjectID(last) {
		return fmt.Errorf("seed %q is not a canonical UUIDv7", last)
	}
	b, _ := hex.DecodeString(strings.ReplaceAll(last, "-", ""))
	g.mu.Lock()
	defer g.mu.Unlock()
	if last > g.last {
		top := binary.BigEndian.Uint64(b[:8])
		g.lastMS, g.hi, g.lo, g.last = top>>16, top&0xfff, binary.BigEndian.Uint64(b[8:])&(1<<62-1), last
	}
	return nil
}

// UUIDv7Time returns the timestamp of a canonical UUIDv7, to the
// millisecond.
func UUIDv7Time(id string) (time.Time, error) {
	if !ValidSubjectID(id) {
		return time.Time{}, fmt.Errorf("%q is not a canonical UUIDv7", id)
	}
	b, _ := hex.DecodeString(strings.ReplaceAll(id[:13], "-", ""))
	var ms int64 // 48 bits, so it can't overflow
	for _, c := range b[:6] {
		ms = ms<<8 | int64(c)
	}
	return time.UnixMilli(ms).UTC(), nil
}
