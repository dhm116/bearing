package resolver

import (
	"context"
	"fmt"
	"maps"
	"slices"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// consolidate moves what the ChangeSet's merges leave under a merged subject
// to its survivor (docs/spec/data-model.md, "Merge", step 3): the source's
// writes about its facts, and the watermarks of its scopes, so that facts
// the two subjects had in common combine by the ordering rule, and each of
// the survivor's facts is ended by the merged subject's scopes. The support
// timelines it moves are rewritten under the survivor and the old ones
// retracted.
//
// It finds the merged subject's facts through the live supports the store
// reads back. A fact whose source has ended it everywhere keeps its writes
// under the merged subject (issue #86 has the same gap for bindings).
func (r *factRun) consolidate(ctx context.Context) error {
	var merged []string
	for m := range r.g.merged {
		if !isRef(m) {
			merged = append(merged, m)
		}
	}
	slices.Sort(merged)
	for _, m := range merged {
		to, err := r.g.canon(ctx, m)
		if err != nil {
			return err
		}
		if err := r.moveSupports(ctx, m); err != nil {
			return err
		}
		if err := r.moveScopes(ctx, m, to); err != nil {
			return err
		}
	}
	return nil
}

// moveSupports re-keys the writes of every fact the merged subject is an end
// of to the canonical fact, joining the writes already there.
func (r *factRun) moveSupports(ctx context.Context, m string) error {
	tls, err := r.supportsOf(ctx, m)
	if err != nil {
		return err
	}
	for _, st := range tls {
		w, err := written(st)
		if err != nil {
			return err
		}
		n, err := r.canonicalFact(ctx, st)
		if err != nil {
			return err
		}
		if w.id() == n.id() {
			continue
		}
		source := st.GetSource()
		old, err := r.readSeries(ctx, source, w)
		if err != nil {
			return err
		}
		next, err := r.readSeries(ctx, source, n)
		if err != nil {
			return err
		}
		for _, e := range old.s {
			next.s = next.s.overlay(e)
		}
		next.dirty = true
		old.s, old.dirty = nil, true
		r.retired[retiredKey(source, w)] = &modelv1alpha1.SupportTimeline{
			Source: source, SubjectId: w.subject, Predicate: w.pred, Object: w.object,
		}
		r.touch(source, n)
	}
	return nil
}

// moveScopes moves the watermarks of the merged subject's scopes to the
// survivor, and marks the survivor's facts they end for recomputing. A scope
// can name any predicate a fact can have: "*", every registered predicate and
// every attribute the source declares for any kind, in either direction.
func (r *factRun) moveScopes(ctx context.Context, m, to string) error {
	for _, source := range slices.Sorted(maps.Keys(r.ix.sources)) {
		src := r.ix.sources[source]
		var cands []scope
		for _, in := range []bool{false, true} {
			cands = append(cands, scope{in: in, pred: "*"})
			for _, p := range model.RegisteredPredicates() {
				cands = append(cands, scope{in: in, pred: p})
			}
		}
		for kind := range src.kinds {
			for name := range src.declaredAttributes(kind) {
				cands = append(cands, scope{pred: src.stored(kind, name)})
			}
		}
		var moved []scope
		for _, c := range cands {
			from, err := r.readMarks(ctx, wmKey(source, m, c.in, c.pred), false)
			if err != nil {
				return err
			}
			if len(from.list) == 0 {
				continue
			}
			into, err := r.readMarks(ctx, wmKey(source, to, c.in, c.pred), isRef(to))
			if err != nil {
				return err
			}
			for _, w := range from.list {
				into.add(w)
			}
			from.list, from.dirty = nil, true
			moved = append(moved, c)
		}
		if len(moved) > 0 {
			if err := r.discover(ctx, source, to, moved); err != nil {
				return fmt.Errorf("source %s: %w", source, err)
			}
		}
	}
	return nil
}
