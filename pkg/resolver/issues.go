package resolver

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// Issue timeline keys. A segment may be a ref the ChangeSet mints: the store
// substitutes the subject (docs/spec/contracts.md, "State keys").
const (
	unobservedPrefix = "unobserved_object/"
	idConflictPrefix = "id_conflict/"
)

// issues writes the data-quality issue timelines the ChangeSet changes:
// objects of relations that nothing observes, and pairs the guard excludes
// (docs/spec/data-model.md, "Data quality").
func (r *factRun) issues(ctx context.Context) error {
	objects := map[string]bool{}
	for _, t := range r.touched {
		if o := t.f.objectSubject(); o != "" {
			objects[o] = true
		}
	}
	for s := range r.existsChanged {
		objects[s] = true
	}
	var out []*modelv1alpha1.IssueTimeline
	for _, o := range slices.Sorted(maps.Keys(objects)) {
		it, err := r.unobserved(ctx, r.g.mustCanon(ctx, o))
		if err != nil {
			return err
		}
		out = append(out, it)
	}
	// A subject merged away answers through its survivor.
	for _, m := range slices.Sorted(maps.Keys(r.g.merged)) {
		if !isRef(m) {
			out = append(out, &modelv1alpha1.IssueTimeline{Key: unobservedPrefix + m})
		}
	}
	out = append(out, r.idConflicts(ctx)...)
	// One timeline per key: a survivor named twice is computed twice.
	seen := map[string]bool{}
	for _, it := range out {
		if !seen[it.GetKey()] {
			seen[it.GetKey()] = true
			r.cs.Issues = append(r.cs.Issues, it)
		}
	}
	slices.SortFunc(r.cs.Issues, func(a, b *modelv1alpha1.IssueTimeline) int { return cmp.Compare(a.GetKey(), b.GetKey()) })
	return nil
}

// idConflicts returns the timelines of the id aliases the guard kept apart in
// this apply, one per pair of aliases. An id_conflict covers all valid time:
// it is about which ids the system keeps apart, not when. It is keyed by the
// two aliases, which merges of the subjects holding them do not change, and is
// not retracted when those subjects later merge by co-reported ids, since the
// system then holds two ids for one subject.
func (r *factRun) idConflicts(ctx context.Context) []*modelv1alpha1.IssueTimeline {
	var out []*modelv1alpha1.IssueTimeline
	for _, c := range r.u.idConflicts {
		a, b := r.g.mustCanon(ctx, c.a), r.g.mustCanon(ctx, c.b)
		x, y := c.x.key, c.y.key
		if y < x {
			x, y = y, x
			a, b = b, a
		}
		if r.before(b, a) {
			a, b = b, a
		}
		issue := &modelv1alpha1.DataQualityIssue{
			Issue: modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT, SubjectIds: []string{a, b},
			Aliases: []string{string(x), string(y)}, Supports: []*modelv1alpha1.Support{proto.CloneOf(c.support)},
		}
		out = append(out, &modelv1alpha1.IssueTimeline{
			Key: idConflictPrefix + escape(string(x)) + "/" + escape(string(y)), Spans: []*modelv1alpha1.IssueSpan{{Issue: issue}},
		})
	}
	return out
}

// before orders subjects the way the store's IDs will: existing IDs by text,
// then the subjects this ChangeSet mints in mint order, since the store mints
// each after every ID it has, so a pair is keyed the same before and after
// its refs resolve.
func (r *factRun) before(a, b string) bool {
	rank := func(id string) int {
		if !isRef(id) {
			return -1
		}
		return slices.IndexFunc(r.cs.Mints, func(m *modelv1alpha1.Mint) bool { return m.GetRef() == id })
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra < rb
	}
	return a < b
}

// unobserved returns the timeline of the unobserved_object issue of a
// subject (canonical, or a ref this ChangeSet mints): the valid times at
// which a relation with a conflict policy, or any relation if the subject is
// a placeholder, has a live support while the subject has no live exists
// support.
func (r *factRun) unobserved(ctx context.Context, o string) (*modelv1alpha1.IssueTimeline, error) {
	it := &modelv1alpha1.IssueTimeline{Key: unobservedPrefix + o}
	rels, err := r.incoming(ctx, o)
	if err != nil {
		return nil, err
	}
	placeholder, err := r.placeholder(ctx, o)
	if err != nil {
		return nil, err
	}
	type live struct {
		f   fact
		sup *modelv1alpha1.Support
		iv  interval
	}
	var lives []live
	var all []interval
	for _, id := range slices.Sorted(maps.Keys(rels)) {
		gf := rels[id]
		reg, ok := model.LookupPredicate(gf.f.pred)
		if !placeholder && (!ok || reg.Conflict == modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE) {
			continue
		}
		for _, source := range slices.Sorted(maps.Keys(gf.bySource)) {
			for _, v := range gf.bySource[source] {
				iv := interval{from: fromTimestamp(v.GetValidFrom(), negInf), to: fromTimestamp(v.GetValidTo(), posInf)}
				sup := proto.CloneOf(v)
				sup.Source, sup.FactId = source, cmp.Or(sup.GetFactId(), gf.f.id())
				lives = append(lives, live{f: gf.f, sup: sup, iv: iv})
				all = append(all, iv)
			}
		}
	}
	eg, err := r.group(ctx, o, model.PredicateExists)
	if err != nil {
		return nil, err
	}
	aliases, err := r.aliasesOf(ctx, o)
	if err != nil {
		return nil, err
	}
	for _, iv := range subtract(joinIntervals(all), observedIn(eg)) {
		issue := &modelv1alpha1.DataQualityIssue{Issue: modelv1alpha1.IssueType_ISSUE_TYPE_UNOBSERVED_OBJECT, SubjectIds: []string{o}, Aliases: aliases}
		for _, l := range lives {
			if l.iv.from < iv.to && iv.from < l.iv.to {
				issue.Supports = append(issue.Supports, l.sup)
				if !slices.Contains(issue.SubjectIds, l.f.subject) {
					issue.SubjectIds = append(issue.SubjectIds, l.f.subject)
				}
			}
		}
		slices.Sort(issue.SubjectIds[1:])
		issue.SubjectIds = issue.SubjectIds[:min(len(issue.SubjectIds), contracts.MaxTimelineRows)]
		issue.Supports = issue.Supports[:min(len(issue.Supports), contracts.MaxTimelineRows)]
		it.Spans = append(it.Spans, &modelv1alpha1.IssueSpan{Issue: issue, ValidFrom: timestampAt(iv.from), ValidTo: timestampAt(iv.to)})
	}
	return it, nil
}

// incoming returns the relation facts whose object is o, with the live
// supports of every source, as the ChangeSet will leave them.
func (r *factRun) incoming(ctx context.Context, o string) (map[string]*groupFact, error) {
	g := map[string]*groupFact{}
	entry := func(f fact) *groupFact {
		gf := g[f.id()]
		if gf == nil {
			gf = &groupFact{f: f, bySource: map[string][]*modelv1alpha1.Support{}}
			g[f.id()] = gf
		}
		return gf
	}
	if !isRef(o) {
		tls, err := r.supportsOf(ctx, o)
		if err != nil {
			return nil, err
		}
		for _, st := range tls {
			if st.GetObject().GetSubjectId() == "" {
				continue
			}
			w, err := written(st)
			if err != nil {
				return nil, err
			}
			if _, gone := r.retired[retiredKey(st.GetSource(), w)]; gone {
				continue
			}
			f, err := r.canonicalFact(ctx, st)
			if err != nil {
				return nil, err
			}
			if f.objectSubject() == o {
				gf := entry(f)
				gf.bySource[st.GetSource()] = append(gf.bySource[st.GetSource()], st.GetVersions()...)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(r.out)) {
		st := r.out[k]
		if st.GetObject().GetSubjectId() != o {
			continue
		}
		f, err := written(st)
		if err != nil {
			return nil, err
		}
		gf := entry(f)
		if len(st.GetVersions()) == 0 {
			delete(gf.bySource, st.GetSource())
		} else {
			gf.bySource[st.GetSource()] = st.GetVersions()
		}
	}
	return g, nil
}

// placeholder reports whether the subject was minted by a reference to
// something not yet observed.
func (r *factRun) placeholder(ctx context.Context, o string) (bool, error) {
	if isRef(o) {
		for _, m := range r.cs.Mints {
			if m.GetRef() == o {
				return m.GetRule() == modelv1alpha1.MintRule_MINT_RULE_REFERENCE, nil
			}
		}
		return false, nil
	}
	s, err := r.g.subject(ctx, o)
	if err != nil {
		return false, err
	}
	return s.GetMintedBy().GetRule() == modelv1alpha1.MintRule_MINT_RULE_REFERENCE, nil
}

// aliasesOf returns the aliases bound to a subject once the ChangeSet is
// applied, sorted and capped.
func (r *factRun) aliasesOf(ctx context.Context, o string) ([]string, error) {
	owned, err := r.g.owned(ctx, o)
	if err != nil {
		return nil, err
	}
	rebound := map[string]bool{}
	var out []string
	for _, bt := range r.cs.Bindings {
		rebound[bt.GetAlias()] = true
		for _, b := range bt.GetBindings() {
			if b.GetSubjectId() != "" && r.g.mustCanon(ctx, b.GetSubjectId()) == o {
				out = append(out, bt.GetAlias())
				break
			}
		}
	}
	for _, a := range owned {
		if !rebound[string(a)] {
			out = append(out, string(a))
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	return out[:min(len(out), contracts.MaxTimelineRows)], nil
}

// joinIntervals sorts intervals and joins the ones that touch or overlap.
func joinIntervals(all []interval) []interval {
	slices.SortFunc(all, func(a, b interval) int { return cmp.Compare(a.from, b.from) })
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

// subtract returns what is in a and in none of b; both are sorted and joined.
func subtract(a, b []interval) []interval {
	var out []interval
	for _, iv := range a {
		from := iv.from
		for _, cut := range b {
			if cut.to <= from || cut.from >= iv.to {
				continue
			}
			if cut.from > from {
				out = append(out, interval{from: from, to: cut.from})
			}
			from = cut.to
			if from >= iv.to {
				break
			}
		}
		if from < iv.to {
			out = append(out, interval{from: from, to: iv.to})
		}
	}
	return out
}
