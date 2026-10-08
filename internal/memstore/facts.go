package memstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// writeFacts writes support and fact timelines.
func (s *Store) writeFacts(undo *[]func(), cs *modelv1alpha1.ChangeSet, r time.Time) error {
	seen := map[string]bool{}
	for _, st := range cs.GetSupports() {
		key, err := tripleKey(st.GetSubjectId(), st.GetPredicate(), st.GetObject())
		if err == nil && st.GetSource() == "" {
			err = errors.New("source is required")
		}
		if err != nil {
			return fmt.Errorf("support timeline: %w", err)
		}
		if seen["s"+st.GetSource()+"\x00"+key] {
			return fmt.Errorf("support timeline %s %s: twice in one change set", st.GetSource(), key)
		}
		seen["s"+st.GetSource()+"\x00"+key] = true
		rows := make([]proto.Message, len(st.GetVersions()))
		for i, v := range st.GetVersions() {
			if v.GetConfidencePpm() == 0 || v.GetConfidencePpm() > 1_000_000 || v.GetSource() != st.GetSource() {
				return fmt.Errorf("support %s %s: want the timeline's source and confidence_ppm in [1, 1000000]", st.GetSource(), key)
			}
			rows[i] = v
		}
		head := &modelv1alpha1.SupportTimeline{Source: st.GetSource(), SubjectId: st.GetSubjectId(), Predicate: st.GetPredicate(), Object: st.GetObject()}
		if err := s.write(undo, s.supports, st.GetSource()+"\x00"+key, head, rows, r); err != nil {
			return err
		}
	}
	for _, ft := range cs.GetFacts() {
		key, err := tripleKey(ft.GetSubjectId(), ft.GetPredicate(), ft.GetObject())
		if err != nil {
			return fmt.Errorf("fact timeline: %w", err)
		}
		if seen["f"+key] {
			return fmt.Errorf("fact timeline %s: twice in one change set", key)
		}
		seen["f"+key] = true
		for _, sp := range ft.GetSpans() {
			if sp.GetStatus() == modelv1alpha1.FactStatus_FACT_STATUS_UNSPECIFIED || sp.GetStatusReason() == modelv1alpha1.StatusReason_STATUS_REASON_UNSPECIFIED ||
				sp.GetConfidencePpm() > model.MaxConfidence {
				return fmt.Errorf("fact %s: want a span with a status, a status_reason and confidence_ppm in [0, %d]", key, model.MaxConfidence)
			}
		}
		head := &modelv1alpha1.FactTimeline{SubjectId: ft.GetSubjectId(), Predicate: ft.GetPredicate(), Object: ft.GetObject()}
		if err := s.write(undo, s.facts, key, head, messages(ft.GetSpans()), r); err != nil {
			return err
		}
	}
	return nil
}

func messages[T proto.Message](in []T) []proto.Message {
	out := make([]proto.Message, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}

// tripleKey names a fact as written. It is the fact ID of the uncanonical
// triple, which also checks the object.
func tripleKey(subject, predicate string, object *modelv1alpha1.FactObject) (string, error) {
	if subject == "" || predicate == "" {
		return "", errors.New("subject_id and predicate are required")
	}
	return model.FactID(subject, predicate, object)
}

// canonicalFact returns a timeline's fact with canonical subject and object
// as recorded at r.
func (s *Store) canonicalFact(subject, predicate string, object *modelv1alpha1.FactObject, r time.Time) *modelv1alpha1.Fact {
	object = proto.CloneOf(object)
	if object.GetSubjectId() != "" {
		object.SubjectId = s.canonical(object.GetSubjectId(), r)
	}
	f := &modelv1alpha1.Fact{SubjectId: s.canonical(subject, r), Predicate: predicate, Object: object}
	f.FactId, _ = model.FactID(f.GetSubjectId(), predicate, object) // checked on apply
	return f
}

// Supports implements contracts.GraphStore.
func (s *Store) Supports(_ context.Context, f contracts.SupportFilter, recordedAt time.Time) ([]*modelv1alpha1.SupportTimeline, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, r := s.times(time.Time{}, recordedAt)
	want := s.canonical(string(f.SubjectID), r)
	type entry struct {
		st  *modelv1alpha1.SupportTimeline
		key string // the timeline's key: source and fact ID as written
	}
	var found []entry
	for _, key := range sortedKeys(s.supports) {
		ser := s.supports[key]
		h, _ := ser.head.(*modelv1alpha1.SupportTimeline)
		st := proto.CloneOf(h)
		fact := s.canonicalFact(st.GetSubjectId(), st.GetPredicate(), st.GetObject(), r)
		if f.Predicate != "" && f.Predicate != st.GetPredicate() || f.Source != "" && f.Source != st.GetSource() ||
			want != "" && want != fact.GetSubjectId() && want != fact.GetObject().GetSubjectId() {
			continue
		}
		for _, v := range ser.live(r) {
			sup, _ := stamp(v).(*modelv1alpha1.Support)
			sup.FactId = fact.GetFactId()
			st.Versions = append(st.Versions, sup)
		}
		sortByValidity(st.Versions)
		if len(st.Versions) > 0 {
			found = append(found, entry{st, key})
		}
	}
	// By canonical fact ID, source, then the key, which is the source and
	// the fact ID as written, so timelines that merged into one fact keep a
	// fixed order.
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i].st, found[j].st
		if x, y := a.GetVersions()[0].GetFactId(), b.GetVersions()[0].GetFactId(); x != y {
			return x < y
		}
		if a.GetSource() != b.GetSource() {
			return a.GetSource() < b.GetSource()
		}
		return found[i].key < found[j].key
	})
	var out []*modelv1alpha1.SupportTimeline
	for _, e := range found {
		out = append(out, e.st)
	}
	return out, nil
}

// sortedKeys lists a table's series keys in order, so reads that combine
// series visit them the same way every time.
func sortedKeys(t table) []string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// timeLess orders timestamps, absent (unbounded) first.
func timeLess(a, b *timestamppb.Timestamp) bool {
	switch {
	case a == nil || b == nil:
		return a == nil && b != nil
	default:
		return a.AsTime().Before(b.AsTime())
	}
}

// sortByValidity orders support versions by valid_from, then valid_to,
// keeping the order of equal ones.
func sortByValidity(v []*modelv1alpha1.Support) {
	sort.SliceStable(v, func(i, j int) bool {
		if !timeEqual(v[i].GetValidFrom(), v[j].GetValidFrom()) {
			return timeLess(v[i].GetValidFrom(), v[j].GetValidFrom())
		}
		return timeLess(v[i].GetValidTo(), v[j].GetValidTo())
	})
}

func timeEqual(a, b *timestamppb.Timestamp) bool {
	return a == nil && b == nil || a != nil && b != nil && a.AsTime().Equal(b.AsTime())
}

// factAt is one fact's span at a point, from one timeline.
type factAt struct {
	fact *modelv1alpha1.Fact
	span *modelv1alpha1.FactSpan
	rows []proto.Message // the timeline as recorded at the point
}

// factsAt returns every fact with a span at (v, r), canonicalized as
// recorded at rc, by fact ID. If timelines that canonicalize to one fact
// both have a span there, better picks one; see docs/spec/contracts.md,
// "Reads".
func (s *Store) factsAt(v, r, rc time.Time) map[string]factAt {
	out := map[string]factAt{}
	for _, key := range sortedKeys(s.facts) {
		ser := s.facts[key]
		rows := ser.at(r)
		span, _ := covering(rows, v).(*modelv1alpha1.FactSpan)
		if span == nil {
			continue
		}
		h, _ := ser.head.(*modelv1alpha1.FactTimeline)
		fa := factAt{s.canonicalFact(h.GetSubjectId(), h.GetPredicate(), h.GetObject(), rc), span, rows}
		if old, ok := out[fa.fact.GetFactId()]; !ok || better(span, old.span) {
			out[fa.fact.GetFactId()] = fa
		}
	}
	return out
}

// better reports whether span a beats b: higher confidence, then greater
// status, then greater status_reason, then earlier valid_from, then earlier
// valid_to. Spans equal in all of them tie, and the caller keeps the one
// whose timeline has the smaller key.
func better(a, b *modelv1alpha1.FactSpan) bool {
	switch {
	case a.GetConfidencePpm() != b.GetConfidencePpm():
		return a.GetConfidencePpm() > b.GetConfidencePpm()
	case a.GetStatus() != b.GetStatus():
		return a.GetStatus() > b.GetStatus()
	case a.GetStatusReason() != b.GetStatusReason():
		return a.GetStatusReason() > b.GetStatusReason()
	case !timeEqual(a.GetValidFrom(), b.GetValidFrom()):
		return timeLess(a.GetValidFrom(), b.GetValidFrom())
	default:
		return timeLess(a.GetValidTo(), b.GetValidTo())
	}
}

// supportsAt returns the support versions covering v as recorded at r,
// canonicalized as recorded at rc, by fact ID and sorted by source.
func (s *Store) supportsAt(v, r, rc time.Time) map[string][]*modelv1alpha1.Support {
	out := map[string][]*modelv1alpha1.Support{}
	for _, key := range sortedKeys(s.supports) {
		ser := s.supports[key]
		var fact *modelv1alpha1.Fact
		for _, ver := range ser.live(r) {
			if !covers(ver.msg, v) {
				continue
			}
			if fact == nil {
				h, _ := ser.head.(*modelv1alpha1.SupportTimeline)
				fact = s.canonicalFact(h.GetSubjectId(), h.GetPredicate(), h.GetObject(), rc)
			}
			sup, _ := stamp(ver).(*modelv1alpha1.Support)
			sup.FactId = fact.GetFactId()
			out[fact.GetFactId()] = append(out[fact.GetFactId()], sup)
		}
	}
	for _, sups := range out {
		sortByValidity(sups)
		sort.SliceStable(sups, func(i, j int) bool { return sups[i].GetSource() < sups[j].GetSource() })
	}
	return out
}

// interval widens a span to the maximal interval of rows with its status.
func interval(rows []proto.Message, sp *modelv1alpha1.FactSpan) (from, to *timestamppb.Timestamp) {
	from, to = sp.GetValidFrom(), sp.GetValidTo()
	for grown := true; grown; {
		grown = false
		for _, m := range rows {
			o, _ := m.(*modelv1alpha1.FactSpan)
			if o.GetStatus() != sp.GetStatus() {
				continue
			}
			if from != nil && o.GetValidTo() != nil && o.GetValidTo().AsTime().Equal(from.AsTime()) {
				from, grown = o.GetValidFrom(), true
			}
			if to != nil && o.GetValidFrom() != nil && o.GetValidFrom().AsTime().Equal(to.AsTime()) {
				to, grown = o.GetValidTo(), true
			}
		}
	}
	return from, to
}

// matcher reports whether a canonical fact passes f's subject, key,
// predicate and object; ok is false when f's key resolves to nothing, and
// err is set when f's object is not a valid fact object.
func (s *Store) matcher(f contracts.FactFilter, v, r time.Time) (match func(*modelv1alpha1.Fact) bool, ok bool, err error) {
	subject := ""
	if f.SubjectID != "" {
		subject = s.canonical(string(f.SubjectID), r)
	}
	if f.Key != "" {
		sub, err := s.resolve(f.Key, v, r)
		if err != nil || subject != "" && subject != sub.GetSubjectId() {
			return nil, false, nil
		}
		subject = sub.GetSubjectId()
	}
	objectID := ""
	if f.Object != nil {
		if _, err = model.FactID("-", "-", f.Object); err != nil {
			return nil, false, fmt.Errorf("filter object: %w", err)
		}
		objectID = s.canonicalFact("-", "-", f.Object, r).GetFactId()
	}
	return func(fact *modelv1alpha1.Fact) bool {
		return (subject == "" || fact.GetSubjectId() == subject) && (f.Predicate == "" || fact.GetPredicate() == f.Predicate) &&
			(objectID == "" || s.canonicalFact("-", "-", fact.GetObject(), r).GetFactId() == objectID)
	}, true, nil
}

// AsOf implements contracts.GraphStore.
func (s *Store) AsOf(_ context.Context, f contracts.FactFilter, validAt, recordedAt time.Time) ([]*modelv1alpha1.FactState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, r := s.times(validAt, recordedAt)
	match, ok, err := s.matcher(f, v, r)
	if err != nil {
		return nil, fmt.Errorf("as of: %w", err)
	}
	if !ok {
		return nil, nil
	}
	sups := s.supportsAt(v, r, r)
	var out []*modelv1alpha1.FactState
	for id, fa := range s.factsAt(v, r, r) {
		if !match(fa.fact) || len(f.Statuses) > 0 && !slices.Contains(f.Statuses, fa.span.GetStatus()) {
			continue
		}
		from, to := interval(fa.rows, fa.span)
		out = append(out, &modelv1alpha1.FactState{
			FactId: id, SubjectId: fa.fact.GetSubjectId(), Predicate: fa.fact.GetPredicate(), Object: fa.fact.GetObject(),
			Status: fa.span.GetStatus(), StatusReason: fa.span.GetStatusReason(), ConfidencePpm: proto.Uint32(fa.span.GetConfidencePpm()),
			ValidFrom: from, ValidTo: to, Precision: full(), Supports: sups[id],
		})
	}
	sort.Slice(out, func(i, j int) bool { return factLess(out[i], out[j]) })
	return out, nil
}

func full() *modelv1alpha1.Precision {
	return &modelv1alpha1.Precision{Detail: modelv1alpha1.CompactionDetail_COMPACTION_DETAIL_FULL}
}

type factLike interface {
	GetSubjectId() string
	GetPredicate() string
	GetFactId() string
}

func factLess(a, b factLike) bool {
	if a.GetSubjectId() != b.GetSubjectId() {
		return a.GetSubjectId() < b.GetSubjectId()
	}
	if a.GetPredicate() != b.GetPredicate() {
		return a.GetPredicate() < b.GetPredicate()
	}
	return a.GetFactId() < b.GetFactId()
}

// Changes implements contracts.GraphStore.
func (s *Store) Changes(_ context.Context, f contracts.FactFilter, t1, t2 time.Time, axis contracts.Axis) ([]*modelv1alpha1.FactChange, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v1, v2, r1, r2 time.Time
	switch axis {
	case contracts.AxisValid:
		v1, r1 = s.times(t1, time.Time{})
		v2, r2 = s.times(t2, time.Time{})
	case contracts.AxisRecord:
		_, r1 = s.times(time.Time{}, t1)
		_, r2 = s.times(time.Time{}, t2)
		v1, v2 = r1, r2
	default:
		return nil, fmt.Errorf("changes: unknown axis %d", axis)
	}
	first, second := v1, v2
	if axis == contracts.AxisRecord {
		first, second = r1, r2
	}
	if first.After(second) {
		return nil, errors.New("changes: t1 is after t2")
	}
	match, ok, err := s.matcher(f, v2, r2)
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	if !ok {
		return nil, nil
	}
	before, after := s.factsAt(v1, r1, r2), s.factsAt(v2, r2, r2)
	supBefore, supAfter := s.supportsAt(v1, r1, r2), s.supportsAt(v2, r2, r2)
	ids := map[string]*modelv1alpha1.Fact{}
	for id, fa := range before {
		ids[id] = fa.fact
	}
	for id, fa := range after {
		ids[id] = fa.fact
	}
	var out []*modelv1alpha1.FactChange
	for id, fact := range ids {
		from, to := factPoint(before[id].span), factPoint(after[id].span)
		if !match(fact) || proto.Equal(from, to) {
			continue
		}
		out = append(out, &modelv1alpha1.FactChange{
			FactId: id, SubjectId: fact.GetSubjectId(), Predicate: fact.GetPredicate(), Object: fact.GetObject(),
			From: from, To: to, Precision: full(), SupportsChanged: changedSources(supBefore[id], supAfter[id]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return factLess(out[i], out[j]) })
	return out, nil
}

func factPoint(sp *modelv1alpha1.FactSpan) *modelv1alpha1.FactPoint {
	if sp == nil {
		return &modelv1alpha1.FactPoint{Status: modelv1alpha1.FactStatus_FACT_STATUS_NONE, ConfidencePpm: proto.Uint32(0)}
	}
	return &modelv1alpha1.FactPoint{Status: sp.GetStatus(), ConfidencePpm: proto.Uint32(sp.GetConfidencePpm())}
}

// changedSources lists, sorted, the sources whose supports differ.
func changedSources(a, b []*modelv1alpha1.Support) []string {
	bySource := func(sups []*modelv1alpha1.Support) map[string][]*modelv1alpha1.Support {
		m := map[string][]*modelv1alpha1.Support{}
		for _, sp := range sups {
			m[sp.GetSource()] = append(m[sp.GetSource()], sp)
		}
		return m
	}
	ma, mb := bySource(a), bySource(b)
	var out []string
	for _, m := range []map[string][]*modelv1alpha1.Support{ma, mb} {
		for src := range m {
			if !slices.Contains(out, src) && !slices.EqualFunc(ma[src], mb[src], func(x, y *modelv1alpha1.Support) bool { return proto.Equal(x, y) }) {
				out = append(out, src)
			}
		}
	}
	slices.Sort(out)
	return out
}
