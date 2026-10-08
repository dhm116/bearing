package memstore

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// times fills zero query times with now. A read "now" sees every apply,
// even one whose recorded_at ran ahead of the clock.
func (s *Store) times(v, r time.Time) (time.Time, time.Time) {
	now := s.Now().UTC()
	if v.IsZero() {
		v = now
	}
	if r.IsZero() {
		r = now
		if s.head.After(r) {
			r = s.head
		}
	}
	return v, r
}

func mergeLive(m *modelv1alpha1.MergeRecord, r time.Time) bool {
	return !m.GetRecordedAt().AsTime().After(r) && (m.GetUnmergedAt() == nil || m.GetUnmergedAt().AsTime().After(r))
}

// addMerge appends a merge record and indexes it; it returns the undo.
func (s *Store) addMerge(rec *modelv1alpha1.MergeRecord) func() {
	i := len(s.merges)
	s.merges = append(s.merges, rec)
	s.mergedBy[rec.GetMergedId()] = append(s.mergedBy[rec.GetMergedId()], i)
	s.survivorOf[rec.GetSurvivorId()] = append(s.survivorOf[rec.GetSurvivorId()], i)
	return func() {
		s.merges = s.merges[:i]
		s.mergedBy[rec.GetMergedId()] = s.mergedBy[rec.GetMergedId()][:len(s.mergedBy[rec.GetMergedId()])-1]
		s.survivorOf[rec.GetSurvivorId()] = s.survivorOf[rec.GetSurvivorId()][:len(s.survivorOf[rec.GetSurvivorId()])-1]
	}
}

// indexOfMerge returns the index of rec in merges.
func (s *Store) indexOfMerge(rec *modelv1alpha1.MergeRecord) int {
	for _, i := range s.mergedBy[rec.GetMergedId()] {
		if s.merges[i] == rec {
			return i
		}
	}
	return -1
}

// canonical follows id through the merges recorded at r to an active
// subject.
func (s *Store) canonical(id string, r time.Time) string {
	for moved := true; moved; {
		moved = false
		for _, i := range s.mergedBy[id] {
			if m := s.merges[i]; mergeLive(m, r) {
				id, moved = m.GetSurvivorId(), true
				break
			}
		}
	}
	return id
}

// aliasesOf returns, sorted, every alias with a row as recorded at r that
// maps it to id after canonicalization.
func (s *Store) aliasesOf(id string, r time.Time) []string {
	// The subjects that canonicalize to id: id and, through the merges
	// recorded at r, the ones merged into it.
	subjects := []string{id}
	for i := 0; i < len(subjects); i++ {
		for _, j := range s.survivorOf[subjects[i]] {
			if m := s.merges[j]; mergeLive(m, r) {
				subjects = append(subjects, m.GetMergedId())
			}
		}
	}
	if len(subjects) == 1 {
		return slices.Clone(s.directAliases(id, r))
	}
	found := map[string]bool{}
	for _, sub := range subjects {
		for _, alias := range s.directAliases(sub, r) {
			found[alias] = true
		}
	}
	return slices.Sorted(func(yield func(string) bool) {
		for a := range found {
			if !yield(a) {
				return
			}
		}
	})
}

// directAliases returns, sorted, the aliases with a row as recorded at r
// that maps them to sub as written. At or after the head (every Apply and
// every read of now) it reads liveBy. Earlier it walks aliasesBy, which lists
// every alias that ever had such a row, so for a subject whose aliases moved
// elsewhere it costs the history of those aliases; only reads at an earlier
// record time pay that. During an apply the answer is kept (s.direct) until
// the bindings change, so a ChangeSet pays for it once per subject.
func (s *Store) directAliases(sub string, r time.Time) []string {
	if got, ok := s.direct[sub]; ok {
		return got
	}
	if !r.Before(s.head) {
		// Every row recorded so far is visible at r, so the rows with a
		// current version are the answer, whatever the history.
		out := slices.Sorted(maps.Keys(s.liveBy[sub]))
		if s.direct != nil {
			s.direct[sub] = out
		}
		return out
	}
	var out []string
	for alias := range s.aliasesBy[sub] {
		ser := s.bindings[alias]
		if ser == nil {
			continue
		}
		for _, m := range ser.at(r) {
			if b, _ := m.(*modelv1alpha1.Binding); b.GetSubjectId() == sub {
				out = append(out, alias)
				break
			}
		}
	}
	slices.Sort(out)
	if s.direct != nil {
		s.direct[sub] = out
	}
	return out
}

// indexAlias records that alias has a row for subject, so aliasesOf looks at
// the aliases of the subjects it names, not at every alias. The index only
// grows, except that a rolled-back apply removes what it added; directAliases
// checks the rows themselves.
func (s *Store) indexAlias(subject, alias string) (added bool) {
	if subject == "" || s.aliasesBy[subject][alias] {
		return false
	}
	if s.aliasesBy[subject] == nil {
		s.aliasesBy[subject] = map[string]bool{}
	}
	s.aliasesBy[subject][alias] = true
	return true
}

// relive updates liveBy for alias after its timeline went from old to now
// (either may be nil) and registers the undo.
func (s *Store) relive(undo *[]func(), alias string, old, now *series) {
	subjects := func(ser *series) map[string]bool {
		out := map[string]bool{}
		if ser != nil {
			for _, v := range ser.rows {
				if b, _ := v.msg.(*modelv1alpha1.Binding); v.ret.IsZero() && b.GetSubjectId() != "" {
					out[b.GetSubjectId()] = true
				}
			}
		}
		return out
	}
	was, is := subjects(old), subjects(now)
	set := func(sub string, live bool) {
		if live {
			if s.liveBy[sub] == nil {
				s.liveBy[sub] = map[string]bool{}
			}
			s.liveBy[sub][alias] = true
			return
		}
		delete(s.liveBy[sub], alias)
		if len(s.liveBy[sub]) == 0 {
			delete(s.liveBy, sub)
		}
	}
	var added, dropped []string
	for sub := range is {
		if !was[sub] {
			set(sub, true)
			added = append(added, sub)
		}
	}
	for sub := range was {
		if !is[sub] {
			set(sub, false)
			dropped = append(dropped, sub)
		}
	}
	*undo = append(*undo, func() {
		for _, sub := range added {
			set(sub, false)
		}
		for _, sub := range dropped {
			set(sub, true)
		}
	})
}

// unindexAlias undoes an indexAlias that added an entry.
func (s *Store) unindexAlias(subject, alias string) {
	delete(s.aliasesBy[subject], alias)
	if len(s.aliasesBy[subject]) == 0 {
		delete(s.aliasesBy, subject)
	}
}

// subjectAt returns id as recorded at r, with its status then.
func (s *Store) subjectAt(id string, r time.Time) (*modelv1alpha1.Subject, error) {
	sub, ok := s.subjects[id]
	if !ok || sub.GetMintedAt().AsTime().After(r) {
		return nil, fmt.Errorf("subject %s: %w", id, contracts.ErrNotFound)
	}
	sub = proto.CloneOf(sub)
	for _, i := range s.mergedBy[id] {
		if m := s.merges[i]; mergeLive(m, r) {
			sub.Status, sub.MergedInto = modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED, m.GetSurvivorId()
		}
	}
	return sub, nil
}

// Subject implements contracts.GraphStore.
func (s *Store) Subject(_ context.Context, id contracts.SubjectID, recordedAt time.Time) (*modelv1alpha1.Subject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, r := s.times(time.Time{}, recordedAt)
	return s.subjectAt(string(id), r)
}

// ResolveKey implements contracts.GraphStore.
func (s *Store) ResolveKey(_ context.Context, key model.Key, validAt, recordedAt time.Time) (*modelv1alpha1.Subject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, r := s.times(validAt, recordedAt)
	return s.resolve(key, v, r)
}

func (s *Store) resolve(key model.Key, v, r time.Time) (*modelv1alpha1.Subject, error) {
	if ser, ok := s.bindings[string(key)]; ok {
		if b, _ := covering(ser.at(r), v).(*modelv1alpha1.Binding); b.GetSubjectId() != "" {
			return s.subjectAt(s.canonical(b.GetSubjectId(), r), r)
		}
	}
	return nil, fmt.Errorf("key %s: %w", key, contracts.ErrNotFound)
}

// Bindings implements contracts.GraphStore.
func (s *Store) Bindings(_ context.Context, aliases []model.Key, subjects []contracts.SubjectID, recordedAt time.Time) ([]*modelv1alpha1.Binding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, r := s.times(time.Time{}, recordedAt)
	want := map[string]bool{}
	for _, k := range aliases {
		want[string(k)] = true
	}
	for _, id := range subjects {
		for _, k := range s.aliasesOf(s.canonical(string(id), r), r) {
			want[k] = true
		}
	}
	var out []*modelv1alpha1.Binding
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range want {
			if !yield(k) {
				return
			}
		}
	}) {
		if ser, ok := s.bindings[k]; ok {
			for _, v := range ser.live(r) {
				b, _ := stamp(v).(*modelv1alpha1.Binding)
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// Merges implements contracts.GraphStore.
func (s *Store) Merges(_ context.Context, id contracts.SubjectID, recordedAt time.Time) ([]*modelv1alpha1.MergeRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, r := s.times(time.Time{}, recordedAt)
	var out []*modelv1alpha1.MergeRecord
	idx := slices.Concat(s.survivorOf[string(id)], s.mergedBy[string(id)])
	slices.Sort(idx) // record order
	for _, i := range idx {
		m := s.merges[i]
		if !m.GetRecordedAt().AsTime().After(r) {
			m = proto.CloneOf(m)
			if m.GetUnmergedAt() != nil && m.GetUnmergedAt().AsTime().After(r) {
				m.UnmergedAt, m.UnmergeEventId = nil, ""
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// State implements contracts.GraphStore.
func (s *Store) State(_ context.Context, keys []string, recordedAt time.Time) (map[string]*anypb.Any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, r := s.times(time.Time{}, recordedAt)
	out := map[string]*anypb.Any{}
	for _, k := range keys {
		if ser, ok := s.state[k]; ok {
			if rows := ser.at(r); len(rows) > 0 {
				e, _ := rows[0].(*modelv1alpha1.StateEntry)
				out[k] = proto.CloneOf(e.GetValue())
			}
		}
	}
	return out, nil
}
