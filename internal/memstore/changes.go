package memstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// ChangesWindow resolves the ends of a Changes window against the store's
// clock: the two times the axis compares at, and the record time the later
// end is read at. The same arithmetic reads and the PostgreSQL backend's
// candidate search use, so that they agree on which instants a window holds.
func (s *Store) ChangesWindow(t1, t2 time.Time, axis contracts.Axis) (w1, w2, rc time.Time, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.window(t1, t2, axis)
}

func (s *Store) window(t1, t2 time.Time, axis contracts.Axis) (w1, w2, rc time.Time, err error) {
	switch axis {
	case contracts.AxisValid:
		w1, _ = s.times(t1, time.Time{})
		w2, rc = s.times(t2, time.Time{})
	case contracts.AxisRecord:
		_, w1 = s.times(time.Time{}, t1)
		_, w2 = s.times(time.Time{}, t2)
		rc = w2
	default:
		return w1, w2, rc, fmt.Errorf("changes: unknown axis %d", axis)
	}
	if w1.After(w2) {
		return w1, w2, rc, errors.New("changes: t1 is after t2")
	}
	return w1, w2, rc, nil
}

// Changes implements contracts.GraphStore.
func (s *Store) Changes(_ context.Context, f contracts.FactFilter, t1, t2 time.Time, axis contracts.Axis) ([]*modelv1alpha1.FactChange, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out, _, _, err := s.changes(f, t1, t2, axis)
	return out, err
}

// ChangesPage implements contracts.GraphStore.
func (s *Store) ChangesPage(_ context.Context, r contracts.ChangesRequest) (contracts.ChangesPage, error) {
	if err := r.Check(); err != nil {
		return contracts.ChangesPage{}, fmt.Errorf("changes: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	all, w1, w2, err := s.changes(r.Filter, r.T1, r.T2, r.Axis)
	if err != nil {
		return contracts.ChangesPage{}, err
	}
	page, next := contracts.PageChanges(all, r)
	return contracts.ChangesPage{Changes: page, Next: next, T1: w1, T2: w2}, nil
}

// changes is Changes with the window it resolved, ordered as AsOf is.
func (s *Store) changes(f contracts.FactFilter, t1, t2 time.Time, axis contracts.Axis) (out []*modelv1alpha1.FactChange, w1, w2 time.Time, err error) {
	w1, w2, rc, err := s.window(t1, t2, axis)
	if err != nil {
		return nil, w1, w2, err
	}
	// The valid axis reads both ends as known now; the record axis reads each
	// end as recorded then, at the same valid time.
	v1, r1, v2, r2 := w1, rc, w2, rc
	if axis == contracts.AxisRecord {
		r1 = w1
	}
	match, ok, err := s.matcher(f, v2, r2)
	if err != nil {
		return nil, w1, w2, fmt.Errorf("changes: %w", err)
	}
	if !ok {
		return nil, w1, w2, nil
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
	var byFact map[string]*factTimelines
	for id, fact := range ids {
		from, to := factPoint(before[id].span), factPoint(after[id].span)
		if !match(fact) || proto.Equal(from, to) {
			continue
		}
		if byFact == nil {
			byFact = s.timelinesByFact(r2)
		}
		// The answers differ, so an instant exists; w2 is only the safe side.
		at, _ := byFact[id].lastChange(axis, w1, w2, rc)
		at = cmp.Or(at, w2)
		out = append(out, &modelv1alpha1.FactChange{
			FactId: id, SubjectId: fact.GetSubjectId(), Predicate: fact.GetPredicate(), Object: fact.GetObject(),
			From: from, To: to, Precision: full(), SupportsChanged: changedSources(supBefore[id], supAfter[id]),
			ChangedAt: timestamppb.New(at),
		})
	}
	slices.SortFunc(out, func(a, b *modelv1alpha1.FactChange) int {
		return cmp.Or(strings.Compare(a.GetSubjectId(), b.GetSubjectId()), strings.Compare(a.GetPredicate(), b.GetPredicate()), strings.Compare(a.GetFactId(), b.GetFactId()))
	})
	return out, w1, w2, nil
}

// LastChange implements contracts.GraphStore.
func (s *Store) LastChange(_ context.Context, f contracts.FactFilter, t time.Time, axis contracts.Axis) (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, hi, rc, err := s.window(t, t, axis)
	if err != nil {
		return time.Time{}, err
	}
	match, ok, err := s.matcher(f, hi, rc)
	if err != nil {
		return time.Time{}, fmt.Errorf("last change: %w", err)
	}
	if !ok {
		return time.Time{}, fmt.Errorf("last change: %w", contracts.ErrNotFound)
	}
	// Facts in order of their latest boundary, so that the search can stop at
	// the first one whose boundary is no newer than the best change found.
	type cand struct {
		ft    *factTimelines
		times []time.Time
	}
	var cands []cand
	for _, ft := range s.timelinesByFact(rc) {
		if !match(ft.fact) {
			continue
		}
		if ts := ft.boundaries(axis, time.Time{}, hi); len(ts) > 0 {
			cands = append(cands, cand{ft, ts})
		}
	}
	slices.SortFunc(cands, func(a, b cand) int { return b.times[0].Compare(a.times[0]) })
	var best time.Time
	for _, c := range cands {
		if !best.IsZero() && !c.times[0].After(best) {
			break
		}
		if at, found := c.ft.changeAmong(axis, c.times, rc); found && at.After(best) {
			best = at
		}
	}
	if best.IsZero() {
		return time.Time{}, fmt.Errorf("last change: %w", contracts.ErrNotFound)
	}
	return best, nil
}

// factTimelines are the fact timelines that canonicalize to one fact, in key
// order.
type factTimelines struct {
	fact *modelv1alpha1.Fact
	sers []*series
}

// timelinesByFact groups the fact timelines by the canonical fact they make
// as recorded at rc.
func (s *Store) timelinesByFact(rc time.Time) map[string]*factTimelines {
	out := map[string]*factTimelines{}
	for _, key := range sortedKeys(s.facts) {
		ser := s.facts[key]
		h, _ := ser.head.(*modelv1alpha1.FactTimeline)
		fact := s.canonicalFact(h.GetSubjectId(), h.GetPredicate(), h.GetObject(), rc)
		ft := out[fact.GetFactId()]
		if ft == nil {
			ft = &factTimelines{fact: fact}
			out[fact.GetFactId()] = ft
		}
		ft.sers = append(ft.sers, ser)
	}
	return out
}

// spanAt is the fact's span at valid time v as recorded at r: the same one
// factsAt picks.
func (ft *factTimelines) spanAt(v, r time.Time) *modelv1alpha1.FactSpan {
	var best *modelv1alpha1.FactSpan
	for _, ser := range ft.sers {
		if sp, _ := covering(ser.at(r), v).(*modelv1alpha1.FactSpan); sp != nil && (best == nil || better(sp, best)) {
			best = sp
		}
	}
	return best
}

// answerAt is the fact's status and confidence at instant t on axis: at
// valid time t as known at rc on the valid axis, at valid and record time t
// on the record axis.
func (ft *factTimelines) answerAt(axis contracts.Axis, t, rc time.Time) *modelv1alpha1.FactPoint {
	if axis == contracts.AxisRecord {
		return factPoint(ft.spanAt(t, t))
	}
	return factPoint(ft.spanAt(t, rc))
}

// boundaries lists, newest first and without repeats, the instants in (lo,
// hi] at which the fact's answer on axis could change: where a span starts or
// ends in valid time and, on the record axis, where a row was recorded or
// retracted. A zero lo is no lower bound. It looks at every row ever
// recorded, so it may list instants at which nothing changed.
func (ft *factTimelines) boundaries(axis contracts.Axis, lo, hi time.Time) []time.Time {
	var out []time.Time
	add := func(t time.Time) {
		if t.After(lo) && !t.After(hi) {
			out = append(out, t)
		}
	}
	for _, ser := range ft.sers {
		for _, v := range ser.rows {
			if sp, _ := v.msg.(*modelv1alpha1.FactSpan); sp != nil {
				if sp.GetValidFrom() != nil {
					add(sp.GetValidFrom().AsTime())
				}
				if sp.GetValidTo() != nil {
					add(sp.GetValidTo().AsTime())
				}
			}
			if axis == contracts.AxisRecord {
				add(v.rec)
				if !v.ret.IsZero() {
					add(v.ret)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return b.Compare(a) })
	return slices.CompactFunc(out, time.Time.Equal)
}

// lastChange is the latest instant in (lo, hi] at which the fact's answer
// differs from the instant before.
func (ft *factTimelines) lastChange(axis contracts.Axis, lo, hi, rc time.Time) (time.Time, bool) {
	return ft.changeAmong(axis, ft.boundaries(axis, lo, hi), rc)
}

// changeAmong is lastChange over the instants ts, newest first.
func (ft *factTimelines) changeAmong(axis contracts.Axis, ts []time.Time, rc time.Time) (time.Time, bool) {
	for _, t := range ts {
		if !proto.Equal(ft.answerAt(axis, t, rc), ft.answerAt(axis, t.Add(-time.Nanosecond), rc)) {
			return t, true
		}
	}
	return time.Time{}, false
}
