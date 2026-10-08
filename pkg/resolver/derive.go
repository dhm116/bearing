package resolver

import (
	"context"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// Derived claims (docs/spec/data-model.md, "Derived claims"). A derived
// support's source is core/derive/<rule>/<input source>; it counts in the
// input source's system and is not authoritative. Its state is the rule
// applied to the input's state at each valid time, so the resolver keeps no
// state for it: every apply that touches an input recomputes it from the
// inputs' supports as the ChangeSet leaves them.
const (
	deriveSourcePrefix = "core/derive/"
	ruleCodeowners     = "codeowners"
)

// Default confidences of owned_by derived from a CODEOWNERS file, by the
// file's shape (docs/spec/data-model.md, "CODEOWNERS").
const (
	DefaultCodeownersSoleTeam = 950_000
	DefaultCodeownersMixed    = 700_000
)

// Codeowners is the confidence, in parts per million, of the owned_by claims
// the codeowners rule derives. A zero field takes its default.
type Codeowners struct {
	// SoleTeam is for a file of one rule line, pattern `*`, naming one team.
	SoleTeam uint32
	// Mixed is for any other file with a `*` line: several owners, a person,
	// or path rules too. The default is below the threshold, so the owner is
	// a candidate for a person or another source to confirm.
	Mixed uint32
}

func deriveSource(rule, input string) string { return deriveSourcePrefix + rule + "/" + input }

// derivedFrom returns the input source of a derived source.
func derivedFrom(source string) (input string, ok bool) {
	rest, ok := strings.CutPrefix(source, deriveSourcePrefix)
	if !ok {
		return "", false
	}
	_, input, ok = strings.Cut(rest, "/")
	return input, ok
}

// codeownersRules returns the stored predicate of the attribute that counts
// the rule lines of a CODEOWNERS file, if the source's declaration gives the
// codeowners rule its inputs for a Repository: approves_changes and that
// attribute.
func (s *sourceInfo) codeownersRules() (string, bool) {
	kd := s.kinds[model.KindRepository]
	if !declares(kd, string(model.RelApprovesChanges), modelv1alpha1.Direction_DIRECTION_OUT) ||
		!declares(kd, "codeowners_rules", modelv1alpha1.Direction_DIRECTION_OUT) {
		return "", false
	}
	return s.stored(model.KindRepository, "codeowners_rules"), true
}

// derivation names a rule's input: the source and the subject it reads.
type derivation struct{ subject, source string }

// derive recomputes the derived supports whose inputs the ChangeSet touches,
// and marks their facts for the status step.
func (r *factRun) derive(ctx context.Context) error {
	targets := map[string]derivation{}
	add := func(d derivation) { targets[d.subject+"\x00"+d.source] = d }
	for _, t := range r.touched {
		src := r.ix.sources[t.source]
		if src == nil {
			continue
		}
		if rules, ok := src.codeownersRules(); ok && (t.f.pred == string(model.RelApprovesChanges) || t.f.pred == rules) {
			add(derivation{t.f.subject, t.source})
		}
	}
	for _, k := range slices.Sorted(maps.Keys(targets)) {
		d := targets[k]
		if r.ix.sources[d.source] == nil {
			continue
		}
		if err := r.deriveCodeowners(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// ownerInput is a repository's approves_changes fact with the input
// source's live versions of it.
type ownerInput struct {
	f  fact
	vs []*modelv1alpha1.Support
}

// countVersion is a live version of the codeowners_rules count.
type countVersion struct {
	n   float64
	sup *modelv1alpha1.Support
}

// deriveCodeowners applies the codeowners rule to one repository's inputs
// from one source: owned_by for each owner on a `*` line, with the
// confidence of the file's shape, at each valid time.
func (r *factRun) deriveCodeowners(ctx context.Context, d derivation) error {
	rulesPred, _ := r.ix.sources[d.source].codeownersRules()
	approvals, err := r.group(ctx, d.subject, string(model.RelApprovesChanges))
	if err != nil {
		return err
	}
	counts, err := r.group(ctx, d.subject, rulesPred)
	if err != nil {
		return err
	}
	var owners []ownerInput
	for _, id := range slices.Sorted(maps.Keys(approvals)) {
		gf := approvals[id]
		if vs := gf.bySource[d.source]; len(vs) > 0 && gf.f.objectSubject() != "" {
			owners = append(owners, ownerInput{f: gf.f, vs: vs})
		}
	}
	var rules []countVersion
	for _, id := range slices.Sorted(maps.Keys(counts)) {
		gf := counts[id]
		if gf.f.object.GetType() != modelv1alpha1.ValueType_VALUE_TYPE_FLOAT {
			continue
		}
		for _, v := range gf.bySource[d.source] {
			rules = append(rules, countVersion{n: gf.f.object.GetValue().GetNumberValue(), sup: v})
		}
	}
	derived := deriveSource(ruleCodeowners, d.source)
	segs := map[string]series{}
	cuts := inputCuts(owners, rules)
	for i := 0; i <= len(cuts); i++ {
		from, to := int64(negInf), int64(posInf)
		if i > 0 {
			from = cuts[i-1]
		}
		if i < len(cuts) {
			to = cuts[i]
		}
		if err := r.codeownersAt(ctx, d, derived, owners, rules, from, to, segs); err != nil {
			return err
		}
	}
	return r.writeDerived(ctx, d, derived, owners, segs)
}

// inputCuts returns, sorted, every valid time an input version starts or
// ends.
func inputCuts(owners []ownerInput, rules []countVersion) []int64 {
	var cuts []int64
	add := func(s *modelv1alpha1.Support) {
		for _, t := range []int64{fromTimestamp(s.GetValidFrom(), negInf), fromTimestamp(s.GetValidTo(), posInf)} {
			if t != negInf && t != posInf {
				cuts = append(cuts, t)
			}
		}
	}
	for _, o := range owners {
		for _, v := range o.vs {
			add(v)
		}
	}
	for _, c := range rules {
		add(c.sup)
	}
	slices.Sort(cuts)
	return slices.Compact(cuts)
}

// latest returns the version of vs that covers v with the greatest
// provenance, or nil.
func latest(vs []*modelv1alpha1.Support, v int64) *modelv1alpha1.Support {
	var best *modelv1alpha1.Support
	for _, s := range vs {
		if fromTimestamp(s.GetValidFrom(), negInf) <= v && v < fromTimestamp(s.GetValidTo(), posInf) && (best == nil || provenanceCmp(best, s) < 0) {
			best = s
		}
	}
	return best
}

// provenanceCmp orders supports by the claim that wrote them: the ordering
// key without its content hash, which a support doesn't carry.
func provenanceCmp(a, b *modelv1alpha1.Support) int {
	if c := a.GetObservedAt().AsTime().Compare(b.GetObservedAt().AsTime()); c != 0 {
		return c
	}
	if c := strings.Compare(a.GetObservationId(), b.GetObservationId()); c != 0 {
		return c
	}
	return strings.Compare(a.GetEventId(), b.GetEventId())
}

// starLines returns the qualifiers of a support that are rule lines with the
// pattern `*`.
func starLines(s *modelv1alpha1.Support) []*structpb.Struct {
	var out []*structpb.Struct
	for _, q := range s.GetQualifiers() {
		if q.GetFields()["pattern"].GetStringValue() == "*" {
			out = append(out, q)
		}
	}
	return out
}

// codeownersAt applies the rule over [from, to), where no input starts or
// ends, and adds the owned_by it derives to segs, by fact.
func (r *factRun) codeownersAt(ctx context.Context, d derivation, derived string, owners []ownerInput, rules []countVersion, from, to int64, segs map[string]series) error {
	// The file's rule count; a file with none read, or two counts, has no
	// shape.
	var count *countVersion
	for i, c := range rules {
		if fromTimestamp(c.sup.GetValidFrom(), negInf) > from || from >= fromTimestamp(c.sup.GetValidTo(), posInf) {
			continue
		}
		if count != nil && (count.n != c.n) {
			return nil
		}
		if count == nil || provenanceCmp(count.sup, c.sup) < 0 {
			count = &rules[i]
		}
	}
	if count == nil {
		return nil
	}
	type live struct {
		o   ownerInput
		sup *modelv1alpha1.Support
	}
	var lives, stars []live
	for _, o := range owners {
		if sup := latest(o.vs, from); sup != nil {
			l := live{o, sup}
			lives = append(lives, l)
			if len(starLines(sup)) > 0 {
				stars = append(stars, l)
			}
		}
	}
	conf := r.ix.codeowners.Mixed
	if len(stars) == 0 {
		return nil
	}
	if count.n == 1 && len(lives) == 1 && len(lives[0].sup.GetQualifiers()) == 1 {
		kind, err := r.kindOf(ctx, lives[0].o.f.objectSubject())
		if err != nil {
			return err
		}
		if kind == model.KindTeam {
			conf = r.ix.codeowners.SoleTeam
		}
	}
	for _, l := range stars {
		in := l.sup
		if provenanceCmp(in, count.sup) < 0 {
			in = count.sup
		}
		c := conf
		sup := &modelv1alpha1.Support{
			Source: derived, EventId: in.GetEventId(), ObservationId: in.GetObservationId(), ObservedAt: in.GetObservedAt(),
			ConfidencePpm: &c, Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_DERIVED,
			Via: proto.CloneOf(l.sup.GetVia()), Qualifiers: starLines(l.sup), Evidence: l.sup.GetEvidence(),
		}
		key := &resolverv1alpha1.OrderingKey{ObservedAt: in.GetObservedAt(), ObservationId: in.GetObservationId(), EventId: in.GetEventId()}
		id := l.o.f.id()
		segs[id] = append(segs[id], seg{from: from, to: to, key: key, live: true, sup: sup})
	}
	return nil
}

// writeDerived sets the derived supports of a repository to segs, and ends
// the ones it had that the rule no longer derives.
func (r *factRun) writeDerived(ctx context.Context, d derivation, derived string, owners []ownerInput, segs map[string]series) error {
	desired := map[string]fact{}
	for _, o := range owners {
		s, ok := segs[o.f.id()]
		if !ok {
			continue
		}
		f, err := newFact(d.subject, string(model.RelOwnedBy), proto.CloneOf(o.f.object))
		if err != nil {
			return err
		}
		var versions []*modelv1alpha1.Support
		for _, c := range s.normalized().versions() {
			versions = append(versions, c.support())
		}
		desired[f.id()] = f
		r.setSupport(derived, f, versions)
		r.touch(derived, f)
	}
	if isRef(d.subject) {
		return nil
	}
	existing, err := r.supportsOf(ctx, d.subject)
	if err != nil {
		return err
	}
	for _, st := range existing {
		if st.GetSource() != derived {
			continue
		}
		w, err := written(st)
		if err != nil {
			return err
		}
		if _, gone := r.retired[retiredKey(derived, w)]; gone {
			continue
		}
		n, err := r.canonicalFact(ctx, st)
		if err != nil {
			return err
		}
		switch {
		case n.subject != d.subject:
		case w.id() != n.id():
			// Left under a subject a merge replaced: moved by the rewrite.
			r.retired[retiredKey(derived, w)] = &modelv1alpha1.SupportTimeline{Source: derived, SubjectId: w.subject, Predicate: w.pred, Object: w.object}
			r.touch(derived, n)
		default:
			if _, keep := desired[w.id()]; !keep {
				r.setSupport(derived, w, nil)
				r.touch(derived, w)
			}
		}
	}
	return nil
}
