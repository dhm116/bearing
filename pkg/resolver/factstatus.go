package resolver

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// statuses computes the status timelines of every (subject, predicate) the
// ChangeSet touches, and writes the support timelines, the fact timelines
// and the state to cs.
func (r *factRun) statuses(ctx context.Context) error {
	dirty := map[string][2]string{}
	for _, t := range r.touched {
		dirty[groupKey(t.f.subject, t.f.pred)] = [2]string{t.f.subject, t.f.pred}
	}
	if err := r.expandExists(ctx, dirty); err != nil {
		return err
	}
	var facts []*modelv1alpha1.FactTimeline
	var conflicts []*modelv1alpha1.ConflictTimeline
	emitted := map[string]bool{}
	for _, k := range slices.Sorted(maps.Keys(dirty)) {
		subject, pred := dirty[k][0], dirty[k][1]
		timelines, ct, err := r.groupStatuses(ctx, subject, pred, r.group)
		if err != nil {
			return err
		}
		prior, priorCT, err := r.beforeStatuses(ctx, subject, pred)
		if err != nil {
			return err
		}
		if priorCT, err = r.canonicalConflicts(ctx, priorCT); err != nil {
			return err
		}
		nowCT, err := r.canonicalConflicts(ctx, ct)
		if err != nil {
			return err
		}
		audit, err := statusAudit(subject, pred, prior, priorCT, timelines, nowCT)
		if err != nil {
			return err
		}
		was, err := r.beforeGroup(ctx, subject, pred)
		if err != nil {
			return err
		}
		now, err := r.group(ctx, subject, pred)
		if err != nil {
			return err
		}
		withdrawn, err := withdrawalEntries(was, now)
		if err != nil {
			return err
		}
		// Withdrawals come first: the claims ended, then the statuses followed.
		r.u.audit = append(r.u.audit, withdrawn...)
		r.u.audit = append(r.u.audit, audit...)
		if ct != nil {
			conflicts = append(conflicts, ct)
		}
		for _, ft := range timelines {
			f, err := newFact(ft.GetSubjectId(), ft.GetPredicate(), ft.GetObject())
			if err != nil {
				return err
			}
			emitted[f.id()] = true
			facts = append(facts, ft)
		}
	}
	// Facts under a merged subject are retracted.
	for _, k := range slices.Sorted(maps.Keys(r.retired)) {
		st := r.retired[k]
		w, err := written(st)
		if err != nil {
			return err
		}
		if !emitted[w.id()] {
			emitted[w.id()] = true
			facts = append(facts, &modelv1alpha1.FactTimeline{SubjectId: w.subject, Predicate: w.pred, Object: w.object})
		}
	}
	conflicts = append(conflicts, r.retiredConflicts()...)
	for _, k := range slices.Sorted(maps.Keys(r.out)) {
		r.cs.Supports = append(r.cs.Supports, r.out[k])
	}
	for _, k := range slices.Sorted(maps.Keys(r.retired)) {
		if _, rewritten := r.out[k]; !rewritten {
			r.cs.Supports = append(r.cs.Supports, r.retired[k])
		}
	}
	r.cs.Facts = append(r.cs.Facts, facts...)
	r.cs.Conflicts = append(r.cs.Conflicts, conflicts...)
	if err := r.issues(ctx); err != nil {
		return err
	}
	entries, err := r.stateEntries()
	if err != nil {
		return err
	}
	r.cs.State = append(r.cs.State, entries...)
	return nil
}

// expandExists adds the groups whose facts depend on the existence of an
// object this ChangeSet changes: a relation that can conflict is only a
// candidate while its object is observed (step 5).
func (r *factRun) expandExists(ctx context.Context, dirty map[string][2]string) error {
	for _, t := range slices.SortedFunc(maps.Values(r.touched), func(a, b affected) int { return cmp.Compare(a.f.id()+a.source, b.f.id()+b.source) }) {
		if t.f.pred != model.PredicateExists || isRef(t.f.subject) {
			continue
		}
		before, err := r.observedBefore(ctx, t.f.subject)
		if err != nil {
			return err
		}
		g, err := r.group(ctx, t.f.subject, model.PredicateExists)
		if err != nil {
			return err
		}
		if slices.Equal(before, observedIn(g)) {
			continue
		}
		r.existsChanged[t.f.subject] = true
		tls, err := r.supportsOf(ctx, t.f.subject)
		if err != nil {
			return err
		}
		for _, st := range tls {
			f, err := r.canonicalFact(ctx, st)
			if err != nil {
				return err
			}
			if pred, ok := model.LookupPredicate(f.pred); f.objectSubject() == t.f.subject && ok && (predicateRules{conflict: pred.Conflict, relation: pred.Relation}).needsObserved() {
				dirty[groupKey(f.subject, f.pred)] = [2]string{f.subject, f.pred}
			}
		}
	}
	return nil
}

// observedBefore returns the valid times a subject has a live exists support
// in the store, before the ChangeSet.
func (r *factRun) observedBefore(ctx context.Context, subject string) ([]interval, error) {
	tls, err := r.supportsOf(ctx, subject)
	if err != nil {
		return nil, err
	}
	g := map[string]*groupFact{}
	for _, st := range tls {
		if st.GetPredicate() != model.PredicateExists {
			continue
		}
		f, err := r.canonicalFact(ctx, st)
		if err != nil {
			return nil, err
		}
		if f.subject != subject {
			continue
		}
		gf := g[f.id()]
		if gf == nil {
			gf = &groupFact{f: f, bySource: map[string][]*modelv1alpha1.Support{}}
			g[f.id()] = gf
		}
		gf.bySource[st.GetSource()] = append(gf.bySource[st.GetSource()], st.GetVersions()...)
	}
	return observedIn(g), nil
}

// observedIn returns the valid times a group of exists facts has a live
// support in any source, as sorted, joined intervals.
func observedIn(g map[string]*groupFact) []interval {
	var all []interval
	for _, gf := range g {
		for _, vs := range gf.bySource {
			for _, v := range vs {
				all = append(all, interval{from: fromTimestamp(v.GetValidFrom(), negInf), to: fromTimestamp(v.GetValidTo(), posInf)})
			}
		}
	}
	slices.SortFunc(all, func(a, b interval) int {
		switch {
		case a.from < b.from:
			return -1
		case a.from > b.from:
			return 1
		}
		return 0
	})
	var out []interval
	for _, iv := range all {
		if n := len(out); n > 0 && out[n-1].to >= iv.from {
			out[n-1].to = max(out[n-1].to, iv.to)
			continue
		}
		out = append(out, iv)
	}
	return out
}

// retiredConflicts returns empty conflict timelines for the subjects the
// ChangeSet merges away, for every predicate that can conflict: the survivor
// answers for them now (docs/spec/contracts.md, "GraphStore").
func (r *factRun) retiredConflicts() []*modelv1alpha1.ConflictTimeline {
	var out []*modelv1alpha1.ConflictTimeline
	for _, m := range slices.Sorted(maps.Keys(r.g.merged)) {
		if isRef(m) {
			continue
		}
		for _, p := range model.RegisteredPredicates() {
			if reg, ok := model.LookupPredicate(p); ok && reg.Conflict != modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE {
				out = append(out, &modelv1alpha1.ConflictTimeline{SubjectId: m, Predicate: p})
			}
		}
	}
	return out
}

// groupFn reads the facts of a subject's predicate with their supports: as the
// ChangeSet leaves them (group) or as the store has them (beforeGroup).
type groupFn func(ctx context.Context, subject, pred string) (map[string]*groupFact, error)

// groupStatuses computes the fact timelines of one subject's predicate, and
// the conflict timeline when the predicate has a conflict policy, from the
// facts that read returns.
func (r *factRun) groupStatuses(ctx context.Context, subject, pred string, read groupFn) ([]*modelv1alpha1.FactTimeline, *modelv1alpha1.ConflictTimeline, error) {
	g, err := read(ctx, subject, pred)
	if err != nil {
		return nil, nil, err
	}
	rules := predicateRules{threshold: r.ix.threshold}
	if reg, ok := model.LookupPredicate(pred); ok {
		rules.conflict, rules.relation = reg.Conflict, reg.Relation
	}
	ids := slices.Sorted(maps.Keys(g))
	candidates := make([]candidateFact, len(ids))
	objects := map[string][]interval{}
	for i, id := range ids {
		gf := g[id]
		candidates[i].object = gf.f.token
		for _, source := range slices.Sorted(maps.Keys(gf.bySource)) {
			auth, err := r.authoritative(ctx, source, gf.f)
			if err != nil {
				return nil, nil, err
			}
			for _, v := range gf.bySource[source] {
				candidates[i].supports = append(candidates[i].supports, support{
					group: r.ix.confidenceGroup(source), authoritative: auth, conf: v.GetConfidencePpm(),
					from: fromTimestamp(v.GetValidFrom(), negInf), to: fromTimestamp(v.GetValidTo(), posInf),
				})
			}
		}
		if o := gf.f.objectSubject(); rules.needsObserved() && o != "" {
			eg, err := read(ctx, o, model.PredicateExists)
			if err != nil {
				return nil, nil, err
			}
			objects[gf.f.token] = observedIn(eg)
		}
	}
	spans, disagreements := statusesAndConflicts(rules, candidates, func(object string) []interval { return objects[object] })
	out := make([]*modelv1alpha1.FactTimeline, len(ids))
	for i, id := range ids {
		ft := &modelv1alpha1.FactTimeline{SubjectId: g[id].f.subject, Predicate: pred, Object: g[id].f.object}
		for _, s := range spans[i] {
			ft.Spans = append(ft.Spans, &modelv1alpha1.FactSpan{
				Status: s.status, StatusReason: s.reason, ConfidencePpm: s.conf,
				ValidFrom: timestampAt(s.from), ValidTo: timestampAt(s.to),
			})
		}
		out[i] = ft
	}
	if rules.conflict == modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE {
		return out, nil, nil
	}
	ct := &modelv1alpha1.ConflictTimeline{SubjectId: subject, Predicate: pred}
	for _, d := range disagreements {
		c := &modelv1alpha1.Conflict{
			SubjectId: subject, Predicate: pred, ValidFrom: timestampAt(d.from), ValidTo: timestampAt(d.to), Resolution: d.resolution,
		}
		for _, pos := range d.positions {
			cp := &modelv1alpha1.ConflictPosition{SourceSystem: pos.group, Authority: &modelv1alpha1.Authority{Authoritative: pos.authoritative}}
			for _, i := range pos.facts {
				cp.Objects = append(cp.Objects, g[ids[i]].f.object)
			}
			c.Positions = append(c.Positions, cp)
		}
		ct.Conflicts = append(ct.Conflicts, c)
	}
	return out, ct, nil
}

// statusAudit returns the audit entries for what a ChangeSet changes in one
// (subject, predicate): a fact whose status timeline differs between before
// and after, then the standing conflicts opened and closed. The statuses before
// are the store's, so what a merge changes (a placeholder object that turns
// out to be a real team, two sources that now disagree) is audited here beside
// the merge.
func statusAudit(subject, pred string, before []*modelv1alpha1.FactTimeline, beforeCT *modelv1alpha1.ConflictTimeline, after []*modelv1alpha1.FactTimeline, afterCT *modelv1alpha1.ConflictTimeline) ([]*modelv1alpha1.AuditEntry, error) {
	t := &trail{}
	index := func(fts []*modelv1alpha1.FactTimeline) map[string]*modelv1alpha1.FactTimeline {
		out := make(map[string]*modelv1alpha1.FactTimeline, len(fts))
		for _, ft := range fts {
			f, err := newFact(ft.GetSubjectId(), ft.GetPredicate(), ft.GetObject())
			if err != nil {
				t.fail(err)
				continue
			}
			out[f.id()] = ft
		}
		return out
	}
	b, a := index(before), index(after)
	ids := slices.Sorted(maps.Keys(b))
	for id := range a {
		if _, ok := b[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		t.factStatus(b[id], a[id])
	}
	t.conflicts(subject, pred, beforeCT.GetConflicts(), afterCT.GetConflicts())
	return t.entries, t.err
}

// authoritative reports whether the source's declaration makes the fact's
// field authoritative for the subject's kind (or the object's, for the
// incoming direction).
func (r *factRun) authoritative(ctx context.Context, source string, f fact) (bool, error) {
	src, ok := r.ix.sources[source]
	if !ok {
		return false, nil
	}
	check := func(id string, in bool) (bool, error) {
		kind, err := r.kindOf(ctx, id)
		if err != nil {
			return false, err
		}
		dir := modelv1alpha1.Direction_DIRECTION_OUT
		if in {
			dir = modelv1alpha1.Direction_DIRECTION_IN
		}
		return slices.ContainsFunc(src.kinds[kind].GetFields(), func(fd *modelv1alpha1.FieldDeclaration) bool {
			return src.stored(kind, fd.GetPredicate()) == f.pred && fieldDirection(fd) == dir && fd.GetAuthority().GetAuthoritative()
		}), nil
	}
	if ok, err := check(f.subject, false); ok || err != nil {
		return ok, err
	}
	if o := f.objectSubject(); o != "" {
		return check(o, true)
	}
	return false, nil
}

// kindOf returns the kind of a subject, or of the subject a ref will mint.
func (r *factRun) kindOf(ctx context.Context, id string) (model.Kind, error) {
	if isRef(id) {
		k, ok := r.u.mints[id]
		if !ok {
			return "", fmt.Errorf("ref %s names no mint", id)
		}
		return k, nil
	}
	s, err := r.g.subject(ctx, id)
	if err != nil {
		return "", err
	}
	return model.Kind(s.GetKind()), nil
}
