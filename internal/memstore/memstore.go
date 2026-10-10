// Package memstore is an in-memory GraphStore, VectorIndex and EventLog, the
// reference implementation of the three contracts, and the engine that applies
// the data model's rules. It is production code: the PostgreSQL backend
// loads rows into a scratch Store and runs its operations here, so the rules
// exist once (workingset.go is the API for that). As a backend in its own
// right (mem://) it is for tests and local trials only: it has no size
// limits beyond the contract's per-ChangeSet limits, keeps everything in
// memory and loses it on exit.
package memstore

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// IDSource issues subject IDs, each greater than the last. NewIDAt stamps
// an ID with the apply's record time, never later. Seed moves the source
// past an ID issued elsewhere, as Restore does. *model.UUIDv7Source is one.
type IDSource interface {
	NewIDAt(at time.Time) string
	Seed(last string) error
}

// Store is a concurrency-safe in-memory GraphStore and VectorIndex. Applies
// are serialized by one lock, the store's clock record.
type Store struct {
	// Now is the store's clock and IDs its subject ID source. Set them
	// before first use; New fills real defaults.
	Now func() time.Time
	IDs IDSource

	mu   sync.RWMutex
	head time.Time // the latest apply's recorded_at
	// restoreUntil is the latest record time a Restore in progress accepts:
	// the backup's taken_at.
	restoreUntil time.Time
	lastID       string // the latest minted subject ID
	journal      []*modelv1alpha1.JournalEntry
	events       map[string]int                    // event ID to journal index
	subjects     map[string]*modelv1alpha1.Subject // as minted
	merges       []*modelv1alpha1.MergeRecord      // in record order
	mergedBy     map[string][]int                  // indexes into merges, by merged subject
	survivorOf   map[string][]int                  // indexes into merges, by survivor
	unmerges     []*modelv1alpha1.UnmergeRecord    // in record order
	unmergesBy   map[string][]int                  // indexes into unmerges, by subject and by target
	aliasesBy    map[string]map[string]bool        // aliases that ever had a row for a subject, as written
	liveBy       map[string]map[string]bool        // aliases with a current (not retracted) row for a subject, as written
	direct       map[string][]string               // during an apply: aliasesOf's per-subject scans, until bindings change
	bindings     table                             // by alias
	supports     table                             // by source, subject, predicate, object
	facts        table                             // by subject, predicate, object
	conflict     table                             // by subject, predicate
	issues       table                             // by the resolver's key
	state        table                             // by the resolver's key
	vectors      map[string]contracts.VectorPoint
	// log is the event log, which has its own lock and which Restore leaves alone.
	log *eventLog
}

var _ contracts.GraphStore = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	s := &Store{Now: time.Now, IDs: model.NewUUIDv7Source(time.Now, rand.Reader), vectors: map[string]contracts.VectorPoint{}, log: newEventLog()}
	s.reset()
	return s
}

// reset empties the graph.
func (s *Store) reset() {
	s.head, s.lastID, s.journal, s.merges, s.unmerges = time.Time{}, "", nil, nil, nil
	s.events, s.subjects = map[string]int{}, map[string]*modelv1alpha1.Subject{}
	s.mergedBy, s.survivorOf, s.unmergesBy = map[string][]int{}, map[string][]int{}, map[string][]int{}
	s.aliasesBy, s.liveBy = map[string]map[string]bool{}, map[string]map[string]bool{}
	s.bindings, s.supports, s.facts, s.conflict, s.issues, s.state = table{}, table{}, table{}, table{}, table{}, table{}
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
		// Equal rows are matched by their deterministic encoding, so the
		// comparison is linear in the rows.
		byContent := map[string][]int{}
		for i, m := range fresh {
			k := content(m)
			byContent[k] = append(byContent[k], i)
		}
		for _, v := range old.rows {
			if !v.ret.IsZero() {
				next.rows = append(next.rows, v)
				continue
			}
			k := content(v.msg)
			if idx := byContent[k]; len(idx) > 0 {
				used[idx[0]] = true
				byContent[k] = idx[1:]
				next.rows = append(next.rows, &version{msg: fresh[idx[0]], rec: v.rec})
			} else {
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

// content is a row's identity for replace: its deterministic encoding
// without last_confirmed_at.
func content(m proto.Message) string {
	// Rows that proto.Equal would call equal but that encode differently
	// (-0 and 0 inside a google.protobuf.Value) are retracted and rewritten
	// instead of kept; qualifiers are canonical JSON, so none arise.
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(withoutConfirmation(m))
	if err != nil {
		// An unencodable row (invalid UTF-8) matches nothing; Apply refuses
		// the ChangeSet when it encodes the journal entry.
		return fmt.Sprintf("unencodable %p", m)
	}
	return string(b)
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
