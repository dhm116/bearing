package memstore

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// This file lets a persistent backend reuse the reference store's rules.
// The backend loads the part of its state an operation needs into a
// scratch Store, runs the operation there, and writes back what changed.
// The rules (checks, canonicalization, ordering) stay in one place, so a
// backend can only differ from the reference in how it stores rows.

// Table names one of the store's series tables.
type Table int

// The series tables.
const (
	TableBindings Table = iota
	TableSupports
	TableFacts
	TableState
)

// Tables lists every table, in the order a ChangeSet writes them.
var Tables = []Table{TableBindings, TableSupports, TableFacts, TableState}

func (s *Store) table(t Table) table {
	switch t {
	case TableBindings:
		return s.bindings
	case TableSupports:
		return s.supports
	case TableFacts:
		return s.facts
	default:
		return s.state
	}
}

// Version is one recorded row of a series: live from Rec until Ret (zero:
// still current).
type Version struct {
	Msg      proto.Message
	Rec, Ret time.Time
}

// Series is one timeline's rows, oldest first, and the message that names
// it (a BindingTimeline, SupportTimeline, FactTimeline or StateEntry with
// its rows cleared).
type Series struct {
	Head     proto.Message
	Versions []Version
}

// NewHead returns an empty head message for t.
func NewHead(t Table) proto.Message {
	switch t {
	case TableBindings:
		return &modelv1alpha1.BindingTimeline{}
	case TableSupports:
		return &modelv1alpha1.SupportTimeline{}
	case TableFacts:
		return &modelv1alpha1.FactTimeline{}
	default:
		return &modelv1alpha1.StateEntry{}
	}
}

// NewRow returns an empty row message for t.
func NewRow(t Table) proto.Message {
	switch t {
	case TableBindings:
		return &modelv1alpha1.Binding{}
	case TableSupports:
		return &modelv1alpha1.Support{}
	case TableFacts:
		return &modelv1alpha1.FactSpan{}
	default:
		return &modelv1alpha1.StateEntry{}
	}
}

// SeriesKey returns the key of the series a head names: the alias, the
// source and fact ID as written, the fact ID as written, or the state key.
func SeriesKey(t Table, head proto.Message) (string, error) {
	switch h := head.(type) {
	case *modelv1alpha1.BindingTimeline:
		return h.GetAlias(), nil
	case *modelv1alpha1.SupportTimeline:
		key, err := tripleKey(h.GetSubjectId(), h.GetPredicate(), h.GetObject())
		return h.GetSource() + "\x00" + key, err
	case *modelv1alpha1.FactTimeline:
		return tripleKey(h.GetSubjectId(), h.GetPredicate(), h.GetObject())
	case *modelv1alpha1.StateEntry:
		return h.GetKey(), nil
	}
	return "", fmt.Errorf("table %d: head is a %T", t, head)
}

// ChangeSetKeys returns the series keys cs writes, by table. Keys cs
// would be refused for are left out; Apply reports those.
func ChangeSetKeys(cs *modelv1alpha1.ChangeSet) map[Table][]string {
	out := map[Table][]string{}
	for _, b := range cs.GetBindings() {
		out[TableBindings] = append(out[TableBindings], b.GetAlias())
	}
	for _, st := range cs.GetSupports() {
		if key, err := SeriesKey(TableSupports, &modelv1alpha1.SupportTimeline{Source: st.GetSource(), SubjectId: st.GetSubjectId(), Predicate: st.GetPredicate(), Object: st.GetObject()}); err == nil {
			out[TableSupports] = append(out[TableSupports], key)
		}
	}
	for _, ft := range cs.GetFacts() {
		if key, err := SeriesKey(TableFacts, &modelv1alpha1.FactTimeline{SubjectId: ft.GetSubjectId(), Predicate: ft.GetPredicate(), Object: ft.GetObject()}); err == nil {
			out[TableFacts] = append(out[TableFacts], key)
		}
	}
	for _, e := range cs.GetState() {
		out[TableState] = append(out[TableState], e.GetKey())
	}
	return out
}

// SubjectsIn returns the subject IDs a message names in any subject_id,
// subject_ids, survivor_id, merged_id or merged_into field, refs
// ("new:…") left out, sorted and without repeats.
func SubjectsIn(m proto.Message) []string {
	var out []string
	collect(m.ProtoReflect(), &out)
	slices.Sort(out)
	return slices.Compact(out)
}

func collect(m protoreflect.Message, out *[]string) {
	add := func(v string) {
		if v != "" && !strings.HasPrefix(v, refPrefix) {
			*out = append(*out, v)
		}
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.Kind() == protoreflect.StringKind && !fd.IsList() && !fd.IsMap():
			switch fd.Name() {
			case "subject_id", "survivor_id", "merged_id", "merged_into":
				add(v.String())
			}
		case fd.Kind() == protoreflect.StringKind && fd.IsList() && fd.Name() == "subject_ids":
			for i := range v.List().Len() {
				add(v.List().Get(i).String())
			}
		case fd.Message() == nil || fd.IsMap() || fd.Message().FullName().Parent() == "google.protobuf":
		case fd.IsList():
			for i := range v.List().Len() {
				collect(v.List().Get(i).Message(), out)
			}
		default:
			collect(v.Message(), out)
		}
		return true
	})
}

// LoadPosition sets the head and the last minted subject ID of a scratch
// store.
func (s *Store) LoadPosition(head time.Time, lastID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.head, s.lastID = head, lastID
}

// LoadSubject adds a subject as minted.
func (s *Store) LoadSubject(sub *modelv1alpha1.Subject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subjects[sub.GetSubjectId()] = proto.CloneOf(sub)
}

// LoadMerge appends a merge record; call it in record order.
func (s *Store) LoadMerge(m *modelv1alpha1.MergeRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merges = append(s.merges, proto.CloneOf(m))
}

// LoadSeries adds or replaces a series.
func (s *Store) LoadSeries(t Table, key string, ser Series) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := &series{head: ser.Head}
	for _, v := range ser.Versions {
		next.rows = append(next.rows, &version{msg: v.Msg, rec: v.Rec, ret: v.Ret})
	}
	s.table(t)[key] = next
}

// Series returns a series as stored, retracted rows included.
func (s *Store) Series(t Table, key string) (Series, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ser, ok := s.table(t)[key]
	if !ok {
		return Series{}, false
	}
	out := Series{Head: ser.head}
	for _, v := range ser.rows {
		out.Versions = append(out.Versions, Version{Msg: v.msg, Rec: v.rec, Ret: v.ret})
	}
	return out, true
}

// MergeRecords returns every merge record the store holds, in record order.
func (s *Store) MergeRecords() []*modelv1alpha1.MergeRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.merges)
}

// Position returns the head and the last minted subject ID.
func (s *Store) Position() (time.Time, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head, s.lastID
}

// LastEntry returns the journal entry of the latest apply, or nil.
func (s *Store) LastEntry() *modelv1alpha1.JournalEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.journal) == 0 {
		return nil
	}
	return s.journal[len(s.journal)-1]
}

// Replay applies a journal entry as a Restore does: at the entry's record
// time, with its subject IDs, failing if the apply decides anything
// differently. until is the latest record time it accepts, the backup's
// taken_at.
func (s *Store) Replay(e *modelv1alpha1.JournalEntry, until time.Time) (contracts.ApplyResult, error) {
	if e.GetChangeSet() == nil {
		return contracts.ApplyResult{}, errors.New("journal entry has no change set")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreUntil = until
	defer func() { s.restoreUntil = time.Time{} }()
	return s.apply(e.GetChangeSet(), e)
}

// ResultOf returns the ApplyResult a journal entry records. duplicate is
// set for the repeat of an event already applied.
func ResultOf(e *modelv1alpha1.JournalEntry, duplicate bool) contracts.ApplyResult {
	return result(e, duplicate)
}
