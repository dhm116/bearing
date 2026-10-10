package resolver

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

// State keys of the fact side (see doc.go):
//
//	sup/<source>/<subject>/<predicate>/<object>  SupportSegments: a source's writes about one fact
//	wm/<source>/<subject>/<out|in>/<predicate>   ScopeWatermarks: the endings of a scope
//
// The object is the subject ID of a relation, or "=" and the fact ID of an
// attribute's value. Source-supplied text is percent-encoded.
const (
	supPrefix = "sup/"
	wmPrefix  = "wm/"
)

func supKey(source string, f fact) string {
	return supPrefix + escape(source) + "/" + f.subject + "/" + escape(f.pred) + "/" + f.token
}

func wmKey(source, subject string, in bool, pred string) string {
	dir := "out"
	if in {
		dir = "in"
	}
	return wmPrefix + escape(source) + "/" + subject + "/" + dir + "/" + escape(pred)
}

// seriesEntry is the stored writes of one source about one fact.
type seriesEntry struct {
	source string
	f      fact
	s      series
	dirty  bool
}

// wmEntry is the stored watermarks of one scope.
type wmEntry struct {
	list  []watermark
	dirty bool
}

// affected names a source's support for a fact whose timeline the ChangeSet
// rewrites.
type affected struct {
	source string
	f      fact
}

// groupFact is a fact of a group, with the live support versions of each
// source.
type groupFact struct {
	f        fact
	bySource map[string][]*modelv1alpha1.Support
}

// factRun computes the claim side of a ChangeSet: the supports and fact
// statuses an observation changes, and the state they are computed from.
type factRun struct {
	u  *run
	g  *graph
	ix *index
	p  *prepared
	cs *modelv1alpha1.ChangeSet
	at int64

	series map[string]*seriesEntry
	marks  map[string]*wmEntry
	// touched are the supports to recompute.
	touched map[string]affected
	// out are the support timelines to write, by source and fact ID.
	out map[string]*modelv1alpha1.SupportTimeline
	// claimed are the live writes the observation's own claims make, by source
	// and fact ID: they outrank its own watermarks where they cover.
	claimed map[string][]seg
	// retired are the support timelines, as written, that move to another
	// key: they are written empty.
	retired map[string]*modelv1alpha1.SupportTimeline
	groups  map[string]map[string]*groupFact
	stored  map[string][]*modelv1alpha1.SupportTimeline
	// existsChanged are the subjects whose live exists times the ChangeSet
	// changes: their unobserved_object issues are recomputed.
	existsChanged map[string]bool
}

func newFactRun(u *run, cs *modelv1alpha1.ChangeSet) *factRun {
	return &factRun{
		u: u, g: u.g, ix: u.ix, p: u.p, cs: cs, at: micros(u.p.at),
		series: map[string]*seriesEntry{}, marks: map[string]*wmEntry{}, touched: map[string]affected{},
		claimed: map[string][]seg{}, out: map[string]*modelv1alpha1.SupportTimeline{}, retired: map[string]*modelv1alpha1.SupportTimeline{},
		groups: map[string]map[string]*groupFact{}, stored: map[string][]*modelv1alpha1.SupportTimeline{},
		existsChanged: map[string]bool{},
	}
}

// facts writes the observation's claims, the watermarks of its scopes, the
// merges' consolidation and the statuses that follow to cs.
func (u *run) facts(ctx context.Context) error {
	cl, err := u.claims(ctx)
	if err != nil {
		return err
	}
	r := newFactRun(u, u.cs)
	if err := r.consolidate(ctx); err != nil {
		return err
	}
	entity := u.g.mustCanon(ctx, u.chosen)
	if err := r.scopes(ctx, entity, cl.scopes); err != nil {
		return err
	}
	if err := r.claim(ctx, cl.claims); err != nil {
		return err
	}
	if err := r.recompute(ctx); err != nil {
		return err
	}
	if err := r.derive(ctx); err != nil {
		return err
	}
	return r.statuses(ctx)
}

// readSeries returns the stored writes of a source about a fact.
func (r *factRun) readSeries(ctx context.Context, source string, f fact) (*seriesEntry, error) {
	k := supKey(source, f)
	if e, ok := r.series[k]; ok {
		return e, nil
	}
	e := &seriesEntry{source: source, f: f}
	r.series[k] = e
	if isRef(f.subject) || isRef(f.objectSubject()) {
		return e, nil
	}
	got, err := r.g.states(ctx, k)
	if err != nil {
		return nil, err
	}
	if a := got[k]; a != nil {
		msg := &resolverv1alpha1.SupportSegments{}
		if err := a.UnmarshalTo(msg); err != nil {
			return nil, fmt.Errorf("%w: support segments: %w", ErrCorrupt, err)
		}
		if e.s, err = seriesOf(msg); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// readMarks returns the stored watermarks of a scope.
func (r *factRun) readMarks(ctx context.Context, key string, hasRef bool) (*wmEntry, error) {
	if e, ok := r.marks[key]; ok {
		return e, nil
	}
	e := &wmEntry{}
	r.marks[key] = e
	if hasRef {
		return e, nil
	}
	got, err := r.g.states(ctx, key)
	if err != nil {
		return nil, err
	}
	if a := got[key]; a != nil {
		if e.list, err = unpackWatermarks(a); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// add adds w to a scope's watermarks unless another already ends
// everything it would: one that starts no later and has a key no less.
func (e *wmEntry) add(w watermark) {
	for _, o := range e.list {
		if o.at <= w.at && model.CompareOrderingKeys(o.key, w.key) >= 0 {
			return
		}
	}
	e.list = slices.DeleteFunc(e.list, func(o watermark) bool {
		return w.at <= o.at && model.CompareOrderingKeys(w.key, o.key) >= 0
	})
	e.list = append(e.list, w)
	e.dirty = true
}

// scopes records the watermarks of the observation's scopes and marks the
// facts they end for recomputing.
func (r *factRun) scopes(ctx context.Context, entity string, scopes []scope) error {
	if len(scopes) == 0 {
		return nil
	}
	for _, s := range scopes {
		e, err := r.readMarks(ctx, wmKey(r.p.ev.Source, entity, s.in, s.pred), isRef(entity))
		if err != nil {
			return err
		}
		e.add(watermark{at: s.at, key: r.p.key, reason: s.reason})
	}
	return r.discover(ctx, r.p.ev.Source, entity, scopes)
}

// discover marks the facts of a source that have a scope's subject as their
// subject (out) or object (in) for recomputing.
func (r *factRun) discover(ctx context.Context, source, subject string, scopes []scope) error {
	if isRef(subject) {
		return nil
	}
	tls, err := r.supportsOf(ctx, subject)
	if err != nil {
		return err
	}
	for _, st := range tls {
		if st.GetSource() != source {
			continue
		}
		f, err := r.canonicalFact(ctx, st)
		if err != nil {
			return err
		}
		for _, s := range scopes {
			subjectEnd := f.subject
			if s.in {
				subjectEnd = f.objectSubject()
			}
			if subjectEnd == subject && (s.pred == "*" || s.pred == f.pred) {
				r.touch(source, f)
				break
			}
		}
	}
	return nil
}

func (r *factRun) touch(source string, f fact) {
	r.touched[source+"\x00"+f.id()] = affected{source: source, f: f}
}

// supportsOf reads the support timelines that have the subject at either
// end, once per subject.
func (r *factRun) supportsOf(ctx context.Context, subject string) ([]*modelv1alpha1.SupportTimeline, error) {
	if got, ok := r.stored[subject]; ok {
		return got, nil
	}
	tls, err := r.g.store.Supports(ctx, contracts.SupportFilter{SubjectID: contracts.SubjectID(subject)}, r.g.at)
	if err != nil {
		return nil, fmt.Errorf("read supports of %s: %w", subject, err)
	}
	r.stored[subject] = tls
	return tls, nil
}

// canonicalFact returns the fact of a stored support timeline with its ends
// followed through the merges known and planned.
func (r *factRun) canonicalFact(ctx context.Context, st *modelv1alpha1.SupportTimeline) (fact, error) {
	subject, err := r.g.canon(ctx, st.GetSubjectId())
	if err != nil {
		return fact{}, err
	}
	obj := proto.CloneOf(st.GetObject())
	if obj.GetSubjectId() != "" {
		if obj.SubjectId, err = r.g.canon(ctx, obj.GetSubjectId()); err != nil {
			return fact{}, err
		}
	}
	return newFact(subject, st.GetPredicate(), obj)
}

// written returns the fact of a stored timeline as written.
func written(st *modelv1alpha1.SupportTimeline) (fact, error) {
	return newFact(st.GetSubjectId(), st.GetPredicate(), st.GetObject())
}

// claim overlays the observation's claims on the stored writes.
func (r *factRun) claim(ctx context.Context, claims []*claim) error {
	for _, c := range claims {
		e, err := r.readSeries(ctx, r.p.ev.Source, c.fact)
		if err != nil {
			return err
		}
		sup := r.supportOf(c)
		for _, w := range c.writes(r.at, r.p.key, sup) {
			if first, last, ok := e.s.dropped(w); ok {
				r.noteDropped(droppedFact(r.p.ev.Source, c.fact, r.p.key, first, last))
			}
			e.s = e.s.overlay(w)
			if w.live {
				k := r.p.ev.Source + "\x00" + c.fact.id()
				r.claimed[k] = append(r.claimed[k], w)
			}
		}
		wms, err := r.watermarksFor(ctx, r.p.ev.Source, c.fact)
		if err != nil {
			return err
		}
		e.s = e.s.settled(wms)
		e.dirty = true
		r.touch(r.p.ev.Source, c.fact)
	}
	return nil
}

// supportOf returns the content of the support version the claim writes.
func (r *factRun) supportOf(c *claim) *modelv1alpha1.Support {
	conf := c.conf
	s := &modelv1alpha1.Support{
		Source: r.p.ev.Source, Adapter: cmp.Or(r.p.ev.Adapter, r.p.src.Adapter), EventId: r.p.ev.ID, ObservationId: r.p.obs.GetId(),
		ObservedAt: r.p.obs.GetTime(), ConfidencePpm: &conf, Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT,
		Qualifiers: c.qualifiers, Evidence: r.p.obs.GetData().GetEvidence(),
	}
	via := &modelv1alpha1.Via{Object: string(c.object)}
	for _, k := range r.p.keys {
		via.Subject = append(via.Subject, string(k.key))
	}
	s.Via = via
	return s
}

// watermarksFor returns the watermarks that apply to a source's fact: those
// of the scopes with its subject as the subject (out) and its object as the
// object (in), for its predicate and for all predicates.
func (r *factRun) watermarksFor(ctx context.Context, source string, f fact) ([]watermark, error) {
	var out []watermark
	scopes := []struct {
		subject string
		in      bool
	}{{f.subject, false}}
	if f.objectSubject() != "" {
		scopes = append(scopes, struct {
			subject string
			in      bool
		}{f.objectSubject(), true})
	}
	for _, sc := range scopes {
		for _, pred := range []string{f.pred, "*"} {
			e, err := r.readMarks(ctx, wmKey(source, sc.subject, sc.in, pred), isRef(sc.subject))
			if err != nil {
				return nil, err
			}
			out = append(out, e.list...)
		}
	}
	return out, nil
}

// recompute rebuilds the support timeline of every touched fact from its
// stored writes and the watermarks that apply.
func (r *factRun) recompute(ctx context.Context) error {
	for _, k := range slices.Sorted(maps.Keys(r.touched)) {
		t := r.touched[k]
		e, err := r.readSeries(ctx, t.source, t.f)
		if err != nil {
			return err
		}
		wms, err := r.watermarksFor(ctx, t.source, t.f)
		if err != nil {
			return err
		}
		r.noteDroppedByWatermarks(t, e.s, wms)
		var versions []*modelv1alpha1.Support
		for _, v := range e.s.effective(wms).versions() {
			versions = append(versions, v.support())
		}
		r.setSupport(t.source, t.f, versions)
	}
	return nil
}

// noteDropped records a dropped write once: a claim with an end is two writes
// of the one fact.
func (r *factRun) noteDropped(d DroppedWrite) {
	if !slices.ContainsFunc(r.u.dropped, func(o DroppedWrite) bool {
		return o.Source == d.Source && o.Subject == d.Subject && o.Predicate == d.Predicate && o.Object == d.Object && o.Alias == d.Alias
	}) {
		r.u.dropped = append(r.u.dropped, d)
	}
}

// noteDroppedByWatermarks records the facts this observation's watermarks fail
// to end where they should have: a segment that joins confirmations made on
// both sides of the watermark's key keeps the later key (see
// [series.settled]). A claim of the observation that covers the stretch
// leaves nothing to report, because [series.dropped] skips segments whose
// key is not past the watermark's.
func (r *factRun) noteDroppedByWatermarks(t affected, s series, wms []watermark) {
	for _, w := range wms {
		if model.CompareOrderingKeys(w.key, r.p.key) != 0 {
			continue
		}
		// Where the observation claims the fact itself, its claim outranks its
		// watermark, so only the stretches no claim covers are the watermark's.
		pieces := []seg{ending(w.at, w.key, w.reason)}
		for _, c := range r.claimed[t.source+"\x00"+t.f.id()] {
			var next []seg
			for _, p := range pieces {
				if c.to <= p.from || c.from >= p.to {
					next = append(next, p)
					continue
				}
				if p.from < c.from {
					l := p
					l.to = c.from
					next = append(next, l)
				}
				if c.to < p.to {
					rr := p
					rr.from = c.to
					next = append(next, rr)
				}
			}
			pieces = next
		}
		for _, p := range pieces {
			if first, last, ok := s.dropped(p); ok {
				r.noteDropped(droppedFact(t.source, t.f, r.p.key, first, last))
				return
			}
		}
	}
}

// setSupport records the support versions a source has for a fact.
func (r *factRun) setSupport(source string, f fact, versions []*modelv1alpha1.Support) {
	r.out[source+"\x00"+f.id()] = &modelv1alpha1.SupportTimeline{
		Source: source, SubjectId: f.subject, Predicate: f.pred, Object: f.object, Versions: versions,
	}
	if g, ok := r.groups[groupKey(f.subject, f.pred)]; ok {
		gf := g[f.id()]
		if gf == nil {
			gf = &groupFact{f: f, bySource: map[string][]*modelv1alpha1.Support{}}
			g[f.id()] = gf
		}
		if len(versions) == 0 {
			delete(gf.bySource, source)
		} else {
			gf.bySource[source] = versions
		}
	}
}

func groupKey(subject, pred string) string { return subject + "\x00" + pred }

// group returns the facts of a subject's predicate with the live supports of
// every source, as the ChangeSet will leave them.
func (r *factRun) group(ctx context.Context, subject, pred string) (map[string]*groupFact, error) {
	key := groupKey(subject, pred)
	if g, ok := r.groups[key]; ok {
		return g, nil
	}
	g := map[string]*groupFact{}
	r.groups[key] = g
	if !isRef(subject) {
		tls, err := r.g.store.Supports(ctx, contracts.SupportFilter{SubjectID: contracts.SubjectID(subject), Predicate: pred}, r.g.at)
		if err != nil {
			return nil, fmt.Errorf("read supports of %s %s: %w", subject, pred, err)
		}
		for _, st := range tls {
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
	}
	for _, k := range slices.Sorted(maps.Keys(r.out)) {
		st := r.out[k]
		if st.GetSubjectId() != subject || st.GetPredicate() != pred {
			continue
		}
		f, err := written(st)
		if err != nil {
			return nil, err
		}
		gf := g[f.id()]
		if gf == nil {
			gf = &groupFact{f: f, bySource: map[string][]*modelv1alpha1.Support{}}
			g[f.id()] = gf
		}
		if len(st.GetVersions()) == 0 {
			delete(gf.bySource, st.GetSource())
		} else {
			gf.bySource[st.GetSource()] = st.GetVersions()
		}
	}
	return g, nil
}

// unpackWatermarks decodes a scope's watermarks.
func unpackWatermarks(a *anypb.Any) ([]watermark, error) {
	msg := &resolverv1alpha1.ScopeWatermarks{}
	if err := a.UnmarshalTo(msg); err != nil {
		return nil, fmt.Errorf("%w: watermarks: %w", ErrCorrupt, err)
	}
	out := make([]watermark, 0, len(msg.GetWatermarks()))
	for _, w := range msg.GetWatermarks() {
		if w.GetAt() == nil || w.GetKey() == nil || w.GetReason() == modelv1alpha1.SupportReason_SUPPORT_REASON_UNSPECIFIED {
			return nil, fmt.Errorf("%w: a watermark without a time, key or reason", ErrCorrupt)
		}
		out = append(out, watermark{at: micros(w.GetAt().AsTime()), key: w.GetKey(), reason: w.GetReason()})
	}
	return out, nil
}

// packWatermarks encodes a scope's watermarks, sorted by start.
func packWatermarks(list []watermark) (*anypb.Any, error) {
	slices.SortFunc(list, func(a, b watermark) int {
		if c := cmp.Compare(a.at, b.at); c != 0 {
			return c
		}
		return model.CompareOrderingKeys(a.key, b.key)
	})
	msg := &resolverv1alpha1.ScopeWatermarks{}
	for _, w := range list {
		msg.Watermarks = append(msg.Watermarks, &resolverv1alpha1.Watermark{At: timestamppb.New(timeOf(w.at)), Key: w.key, Reason: w.reason})
	}
	return anypb.New(msg)
}

// stateEntries returns the state entries the run changed, sorted by key.
func (r *factRun) stateEntries() ([]*modelv1alpha1.StateEntry, error) {
	byKey := map[string]*modelv1alpha1.StateEntry{}
	for k, e := range r.series {
		if !e.dirty {
			continue
		}
		entry := &modelv1alpha1.StateEntry{Key: k}
		if len(e.s) > 0 {
			v, err := anypb.New(e.s.state())
			if err != nil {
				return nil, fmt.Errorf("pack support segments: %w", err)
			}
			entry.Value = v
		}
		byKey[k] = entry
	}
	for k, e := range r.marks {
		if !e.dirty {
			continue
		}
		entry := &modelv1alpha1.StateEntry{Key: k}
		if len(e.list) > 0 {
			v, err := packWatermarks(e.list)
			if err != nil {
				return nil, fmt.Errorf("pack watermarks: %w", err)
			}
			entry.Value = v
		}
		byKey[k] = entry
	}
	out := make([]*modelv1alpha1.StateEntry, 0, len(byKey))
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		out = append(out, byKey[k])
	}
	return out, nil
}

func retiredKey(source string, w fact) string { return source + "\x00" + w.id() }
