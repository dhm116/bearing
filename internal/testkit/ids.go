package testkit

import (
	"fmt"
	"sync/atomic"
	"time"

	"bearing.example/pkg/model"
)

// IDs mints identifiers, such as subject IDs and event IDs.
type IDs interface {
	NewID() string
}

// SeqIDs is a deterministic [IDs]: the nth call to NewID returns the same
// value in every run. It is safe for concurrent use.
type SeqIDs struct {
	prefix string
	uuid   bool
	n      atomic.Uint64
}

// NewSeqIDs returns IDs of the form prefix + "1", prefix + "2", and so on,
// for example "evt-1".
func NewSeqIDs(prefix string) *SeqIDs {
	return &SeqIDs{prefix: prefix}
}

// NewUUIDs returns IDs shaped like version 4 UUIDs whose last group is the
// sequence number, for code that checks the UUID format:
// "00000000-0000-4000-8000-000000000001", then ...0002, and so on.
func NewUUIDs() *SeqIDs {
	return &SeqIDs{uuid: true}
}

// NewID returns the next identifier in the sequence.
func (s *SeqIDs) NewID() string {
	n := s.n.Add(1)
	if s.uuid {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
	}
	return fmt.Sprintf("%s%d", s.prefix, n)
}

// NewUUIDv7s returns a UUIDv7 subject ID source on the clock now whose
// random bits are all zero, so the same clock readings give the same IDs:
// "<ms>-7000-8000-000000000000", then ...0001 within the millisecond.
func NewUUIDv7s(now func() time.Time) *model.UUIDv7Source {
	return model.NewUUIDv7Source(now, zeros{})
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
