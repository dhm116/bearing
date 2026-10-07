package model

import (
	"encoding/binary"
	"encoding/hex"
	"io"
	"sync"
	"time"
)

// UUIDv7 returns a generator of subject IDs: UUIDv7 text (RFC 9562) from a
// monotonic generator, so each ID is greater than the one before, even
// within a millisecond or if the clock steps back (RFC 9562 §6.2, method 2:
// the random bits of the last ID plus one). It panics if rnd fails.
func UUIDv7(now func() time.Time, rnd io.Reader) func() string {
	var (
		mu     sync.Mutex
		lastMS uint64
		hi, lo uint64 // the 12 bits of rand_a and the 62 of rand_b
	)
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		ms := uint64(now().UnixMilli())
		if ms > lastMS {
			var b [16]byte
			if _, err := io.ReadFull(rnd, b[:]); err != nil {
				panic("model: read random bits for a UUIDv7: " + err.Error())
			}
			lastMS, hi, lo = ms, binary.BigEndian.Uint64(b[:8])&0xfff, binary.BigEndian.Uint64(b[8:])&(1<<62-1)
		} else if lo++; lo == 1<<62 {
			if lo, hi = 0, hi+1; hi == 1<<12 {
				hi, lastMS = 0, lastMS+1
			}
		}
		var u [16]byte
		binary.BigEndian.PutUint64(u[:8], lastMS<<16|0x7000|hi)
		binary.BigEndian.PutUint64(u[8:], 1<<63|lo)
		s := hex.EncodeToString(u[:])
		return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
	}
}
