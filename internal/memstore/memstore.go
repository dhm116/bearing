// Package memstore is an in-memory GraphStore and VectorIndex for tests and
// local trials. It is the reference implementation of both contracts.
package memstore

import (
	"crypto/rand"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Store is a concurrency-safe in-memory GraphStore and VectorIndex. Applies
// are serialized by one lock, the store's clock record.
type Store struct {
	// Now is the store's clock. NewID mints subject IDs, each greater than
	// the last. Set them before first use; New fills real defaults.
	Now   func() time.Time
	NewID func() string

	mu       sync.RWMutex
	head     time.Time // the latest apply's recorded_at
	lastID   string    // the latest minted subject ID
	journal  []*modelv1alpha1.ChangeSet
	events   map[string]time.Time
	subjects map[string]*modelv1alpha1.Subject // as minted
	merges   []*modelv1alpha1.MergeRecord      // in record order
	bindings table                             // by alias
	state    table                             // by the resolver's key
	vectors  map[string]contracts.VectorPoint
}

var _ contracts.GraphStore = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	s := &Store{Now: time.Now, NewID: model.UUIDv7(time.Now, rand.Reader), vectors: map[string]contracts.VectorPoint{}}
	s.reset()
	return s
}

// reset empties the graph.
func (s *Store) reset() {
	s.head, s.lastID, s.journal, s.merges = time.Time{}, "", nil, nil
	s.events, s.subjects = map[string]time.Time{}, map[string]*modelv1alpha1.Subject{}
	s.bindings, s.state = table{}, table{}
}

// A table holds series of bitemporal rows. A series is one timeline
// (an alias's bindings, a fact's spans, …); head is the timeline message
// with its rows cleared, which names the series.
type table map[string]*series

type series struct {
	head proto.Message
	rows []*version
}

// version is one row as recorded from rec until ret (zero: still current).
// Versions are never changed in place, so a rollback can restore slices.
type version struct {
	msg      proto.Message
	rec, ret time.Time
}

func (v *version) liveAt(r time.Time) bool {
	return !v.rec.After(r) && (v.ret.IsZero() || v.ret.After(r))
}

// at returns the series' rows as recorded at r.
func (s *series) at(r time.Time) []proto.Message {
	var out []proto.Message
	for _, v := range s.live(r) {
		out = append(out, v.msg)
	}
	return out
}

// live returns the series' versions as recorded at r.
func (s *series) live(r time.Time) []*version {
	var out []*version
	for _, v := range s.rows {
		if v.liveAt(r) {
			out = append(out, v)
		}
	}
	return out
}

// replace makes rows the series' current timeline at record time r: current
// rows equal to a new one (record-time fields and last_confirmed_at aside)
// stay, the others are retracted, and the rest are recorded. It returns a
// function that undoes it.
func (t table) replace(key string, head proto.Message, rows []proto.Message, r time.Time) func() {
	old, existed := t[key]
	next := &series{head: head}
	if existed {
		next.head = old.head
	}
	fresh := make([]proto.Message, len(rows))
	for i, m := range rows {
		fresh[i] = clearRecordFields(m)
	}
	used := make([]bool, len(fresh))
	if existed {
		for _, v := range old.rows {
			if !v.ret.IsZero() {
				next.rows = append(next.rows, v)
				continue
			}
			kept := false
			for i, m := range fresh {
				if !used[i] && proto.Equal(withoutConfirmation(v.msg), withoutConfirmation(m)) {
					used[i], kept = true, true
					next.rows = append(next.rows, &version{msg: m, rec: v.rec})
					break
				}
			}
			if !kept {
				next.rows = append(next.rows, &version{msg: v.msg, rec: v.rec, ret: r})
			}
		}
	}
	for i, m := range fresh {
		if !used[i] {
			next.rows = append(next.rows, &version{msg: m, rec: r})
		}
	}
	t[key] = next
	return func() {
		if existed {
			t[key] = old
		} else {
			delete(t, key)
		}
	}
}

var recordFields = map[protoreflect.Name]bool{"recorded_at": true, "retracted_at": true, "fact_id": true}

// clearRecordFields returns a copy of m without the fields the store sets.
func clearRecordFields(m proto.Message) proto.Message {
	m = proto.Clone(m)
	r := m.ProtoReflect()
	r.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if recordFields[fd.Name()] {
			r.Clear(fd)
		}
		return true
	})
	return m
}

// withoutConfirmation drops last_confirmed_at, which a confirming claim
// changes without making a new support version.
func withoutConfirmation(m proto.Message) proto.Message {
	if s, ok := m.(*modelv1alpha1.Support); ok && s.GetLastConfirmedAt() != nil {
		s = proto.CloneOf(s)
		s.LastConfirmedAt = nil
		return s
	}
	return m
}

// validity is a row with a valid-time interval; rows without one cover all
// valid time.
type validity interface {
	GetValidFrom() *timestamppb.Timestamp
	GetValidTo() *timestamppb.Timestamp
}

func covers(m proto.Message, v time.Time) bool {
	iv, ok := m.(validity)
	if !ok {
		return true
	}
	return (iv.GetValidFrom() == nil || !iv.GetValidFrom().AsTime().After(v)) &&
		(iv.GetValidTo() == nil || iv.GetValidTo().AsTime().After(v))
}

// covering returns the row of rows that covers v, or nil.
func covering(rows []proto.Message, v time.Time) proto.Message {
	for _, m := range rows {
		if covers(m, v) {
			return m
		}
	}
	return nil
}

// stamp returns a copy of v's row with its recorded_at set, for a read.
// Reads return only rows live at the read's record time, so retracted_at
// stays empty.
func stamp(v *version) proto.Message {
	m := proto.Clone(v.msg)
	r := m.ProtoReflect()
	if fd := r.Descriptor().Fields().ByName("recorded_at"); fd != nil {
		r.Set(fd, protoreflect.ValueOfMessage(timestamppb.New(v.rec).ProtoReflect()))
	}
	return m
}
