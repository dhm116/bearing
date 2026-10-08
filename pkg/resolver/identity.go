package resolver

import (
	"context"
	"fmt"
	"slices"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// entityRef is the ref of the subject an observation mints for its entity.
const entityRef = "new:e"

// run is the state of resolving one observation: what the graph says, what
// the ChangeSet has decided so far, and the engine that turns binding writes
// into timelines.
type run struct {
	g   *graph
	eng *engine
	ix  *index
	p   *prepared
	cs  *modelv1alpha1.ChangeSet

	// chosen is the subject the entity resolved to: an ID, or the ref of
	// the mint.
	chosen string
	// mints maps each ref the ChangeSet mints to its kind.
	mints map[string]model.Kind
	// timelines holds the binding timelines decided outside the engine
	// (id aliases), by alias.
	timelines map[model.Key]*modelv1alpha1.BindingTimeline
	// seeds are the name aliases whose writes changed, or whose subjects
	// merged.
	seeds []keyRef
	// merges are the merges to apply, in order.
	merges []*modelv1alpha1.Merge
	// placeholders counts the placeholders minted, to name their refs.
	placeholders int
	// pendingOwners are subjects of planned merges whose names now compete.
	pendingOwners []string
	// consolidate lists the (survivor, merged) pairs whose deletion marks
	// are still to be moved.
	consolidate [][2]string
	// rejections are refusals made while resolving.
	rejections []Rejection
}

// identify resolves the observed entity and its references to subjects and
// writes the mints, bindings, merges and binding state to cs. It returns the
// rejections it made; a rejected observation leaves cs without changes.
func (r *Resolver) identify(ctx context.Context, g *graph, p *prepared, cs *modelv1alpha1.ChangeSet) ([]Rejection, error) {
	u := &run{
		g: g, eng: newEngine(g, r.ix), ix: r.ix, p: p, cs: cs,
		mints: map[string]model.Kind{}, timelines: map[model.Key]*modelv1alpha1.BindingTimeline{},
	}
	rejected, err := u.resolveEntity(ctx)
	if err != nil || rejected {
		return u.rejections, err
	}
	if err := u.writeBindings(ctx); err != nil {
		return nil, err
	}
	if err := u.resolveReferences(ctx); err != nil {
		return nil, err
	}
	if err := u.finish(ctx); err != nil {
		return nil, err
	}
	return u.rejections, nil
}

func (u *run) reject(code modelv1alpha1.RejectionCode, scope model.Scope, path, format string, args ...any) {
	u.rejections = append(u.rejections, Rejection{Code: code, Scope: scope, Path: path, Message: fmt.Sprintf(format, args...)})
}

// idKeys and nameKeys split the entity's keys by class.
func (u *run) idKeys() []keyRef   { return u.keysOf(keyRef.isID) }
func (u *run) nameKeys() []keyRef { return u.keysOf(keyRef.isName) }

func (u *run) keysOf(is func(keyRef) bool) []keyRef {
	var out []keyRef
	for _, k := range u.p.keys {
		if is(k) {
			out = append(out, k)
		}
	}
	return out
}

func aliasesOf(keys []keyRef) []model.Key {
	out := make([]model.Key, len(keys))
	for i, k := range keys {
		out[i] = k.key
	}
	return out
}

// resolveEntity picks the subject for the observed entity: id match, name
// match, or mint (docs/spec/data-model.md, "Resolution"). It reports true if
// it rejected the observation.
func (u *run) resolveEntity(ctx context.Context) (bool, error) {
	ids, names := u.idKeys(), u.nameKeys()
	if err := u.g.load(ctx, aliasesOf(u.p.keys)...); err != nil {
		return false, err
	}
	// 1. Id match.
	found := map[string]bool{}
	for _, k := range ids {
		if rows := u.g.rows[k.key]; len(rows) > 0 {
			c, err := u.g.canon(ctx, rows[0].GetSubjectId())
			if err != nil {
				return false, err
			}
			found[c] = true
		}
	}
	matched := slices.Sorted(keysOf(found))
	for _, id := range matched {
		kind, err := u.g.kindOf(ctx, id, nil)
		if err != nil {
			return false, err
		}
		if kind != u.p.kind {
			u.reject(modelv1alpha1.RejectionCode_REJECTION_CODE_KIND_MISMATCH, model.ScopeObservation, "data.entity.key",
				"an id alias is bound to a %s, and the entity is a %s", kind, u.p.kind)
			return true, nil
		}
	}
	if len(matched) > 0 {
		u.chosen = matched[0]
		for _, other := range matched[1:] {
			// A set distinct_from would reject this (identity_conflict);
			// only manual operations set one, and they come later.
			u.merge(u.chosen, other, modelv1alpha1.MergeRule_MERGE_RULE_CO_REPORTED_IDS)
		}
		return false, nil
	}
	// 2. Name match. A subject found through a tentative binding is a
	// placeholder, adopted only if no observed binding names a subject.
	observed, tentative := map[string]bool{}, map[string]bool{}
	for _, k := range names {
		b := covering(u.g.rows[k.key], u.p.at)
		if b == nil || b.GetSubjectId() == "" {
			continue
		}
		c, err := u.g.canon(ctx, b.GetSubjectId())
		if err != nil {
			return false, err
		}
		reused, err := u.nameReused(ctx, c, ids)
		if err != nil {
			return false, err
		}
		switch {
		case reused:
		case b.GetTentative():
			tentative[c] = true
		default:
			observed[c] = true
		}
	}
	switch {
	case len(observed) > 0:
		// Two subjects would open a conflict on (subject, same_as); the
		// lower subject_id is used and nothing merges.
		u.chosen = slices.Sorted(keysOf(observed))[0]
	case len(tentative) > 0:
		u.chosen = slices.Sorted(keysOf(tentative))[0]
	default:
		// 3. Mint.
		u.cs.Mints = append(u.cs.Mints, &modelv1alpha1.Mint{Ref: entityRef, Kind: string(u.p.kind), Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION})
		u.mints[entityRef] = u.p.kind
		u.chosen = entityRef
	}
	return false, nil
}

// nameReused reports whether subject s holds an id alias of a key type the
// observation also carries, with a different value: the name was reused by a
// new entity.
func (u *run) nameReused(ctx context.Context, s string, ids []keyRef) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	held, err := u.g.owned(ctx, s)
	if err != nil {
		return false, err
	}
	for _, a := range held {
		h, ok := u.ix.lookup(string(a))
		if !ok || !h.isID() {
			continue
		}
		for _, k := range ids {
			if k.groupKey() == h.groupKey() && k.key != h.key {
				return true, nil
			}
		}
	}
	return false, nil
}

// merge plans a merge of two subjects. The lower ID survives, and refs,
// which the store mints after every existing ID, are greater than any ID.
func (u *run) merge(a, b string, rule modelv1alpha1.MergeRule) {
	survivor, merged := a, b
	if lessID(b, a) {
		survivor, merged = b, a
	}
	u.merges = append(u.merges, &modelv1alpha1.Merge{SubjectIds: []string{survivor, merged}, Rule: rule, ConfidencePpm: 1_000_000})
	u.g.plan(merged, survivor)
	u.consolidate = append(u.consolidate, [2]string{survivor, merged})
	// The names of both subjects now compete for one.
	for _, id := range []string{survivor, merged} {
		u.seedOwners(id)
	}
}

// lessID orders subjects the way the store's merge picks a survivor.
func lessID(a, b string) bool {
	switch {
	case isRef(a) != isRef(b):
		return isRef(b)
	default:
		return a < b
	}
}

// seedOwners adds the names bound to a subject to the seeds, once the graph
// is read; the merge's names are looked up in finish.
func (u *run) seedOwners(id string) { u.pendingOwners = append(u.pendingOwners, id) }

// writeBindings binds the entity's keys at observed_at.
func (u *run) writeBindings(ctx context.Context) error {
	p := u.p
	for _, k := range u.idKeys() {
		rows := u.g.rows[k.key]
		switch {
		case len(rows) == 0:
			u.timelines[k.key] = &modelv1alpha1.BindingTimeline{Alias: string(k.key), Bindings: []*modelv1alpha1.Binding{{Alias: string(k.key), SubjectId: u.chosen}}}
		case rows[0].GetTentative():
			// An observation of the entity makes a placeholder's id binding
			// an observed one.
			b := rows[0]
			u.timelines[k.key] = &modelv1alpha1.BindingTimeline{Alias: string(k.key), Bindings: []*modelv1alpha1.Binding{{Alias: string(k.key), SubjectId: b.GetSubjectId()}}}
		}
	}
	if p.obs.GetData().GetEntity().GetDeleted() {
		return u.releaseNames(ctx)
	}
	for _, k := range u.nameKeys() {
		n, err := u.eng.name(ctx, k)
		if err != nil {
			return err
		}
		n.add(write{key: p.key, from: p.at, subject: u.chosen})
		u.seeds = append(u.seeds, k)
	}
	return nil
}

// releaseNames records the entity's deletion in each namespace the source
// reads or issues: from observed_at on, the names the entity holds there
// are released, whenever the writes that bound them are applied
// (docs/spec/data-model.md, "Normalization"). The names it holds now are
// recomputed.
func (u *run) releaseNames(ctx context.Context) error {
	allowed := u.p.src.entityNamespaces()
	for _, ns := range slices.Sorted(keysOf(allowed)) {
		if err := u.eng.addMark(ctx, ns, u.chosen, mark{at: u.p.at, key: u.p.key}); err != nil {
			return err
		}
	}
	if isRef(u.chosen) {
		return nil
	}
	owned, err := u.g.owned(ctx, u.chosen)
	if err != nil {
		return err
	}
	for _, a := range owned {
		if k, ok := u.ix.lookup(string(a)); ok && k.isName() && allowed[k.ns] {
			u.seeds = append(u.seeds, k)
		}
	}
	return nil
}

// resolveReferences resolves the other end of each relation and each linked
// ID at observed_at, minting a placeholder with a tentative binding for one
// that resolves to nothing (docs/spec/data-model.md, "Resolution").
func (u *run) resolveReferences(ctx context.Context) error {
	var refs []keyRef
	for _, r := range u.p.relations {
		refs = append(refs, r.other)
	}
	refs = append(refs, u.p.links...)
	slices.SortFunc(refs, func(a, b keyRef) int { return compareKeys(a.key, b.key) })
	refs = slices.CompactFunc(refs, func(a, b keyRef) bool { return a.key == b.key })
	if err := u.g.load(ctx, aliasesOf(refs)...); err != nil {
		return err
	}
	for _, k := range refs {
		if u.entityHas(k) {
			continue
		}
		subject, tentative, err := u.lookupReference(ctx, k)
		if err != nil {
			return err
		}
		switch {
		case subject == "":
			if err := u.mintPlaceholder(ctx, k); err != nil {
				return err
			}
		case tentative && k.isName():
			// A later reference moves the tentative binding's key forward,
			// which is the latest valid time a reference resolved through
			// it.
			n, err := u.eng.name(ctx, k)
			if err != nil {
				return err
			}
			n.add(write{key: u.p.key, subject: subject, tentative: true})
			u.seeds = append(u.seeds, k)
		}
	}
	return nil
}

func compareKeys(a, b model.Key) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// entityHas reports whether k is one of the observed entity's own keys.
func (u *run) entityHas(k keyRef) bool {
	return slices.ContainsFunc(u.p.keys, func(own keyRef) bool { return own.key == k.key })
}

// lookupReference finds the subject a reference key maps to at observed_at
// in the graph as read, and whether it does so through a tentative binding.
// It returns "" if the key is unbound, or released without a redirect.
func (u *run) lookupReference(ctx context.Context, k keyRef) (string, bool, error) {
	rows := u.g.rows[k.key]
	var b *modelv1alpha1.Binding
	if k.isID() {
		if len(rows) > 0 {
			b = rows[0]
		}
	} else {
		b = covering(rows, u.p.at)
	}
	if b == nil || b.GetSubjectId() == "" {
		return "", false, nil
	}
	c, err := u.g.canon(ctx, b.GetSubjectId())
	return c, b.GetTentative(), err
}

// mintPlaceholder mints a subject for a reference to nothing and binds the
// key to it tentatively.
func (u *run) mintPlaceholder(ctx context.Context, k keyRef) error {
	ref := fmt.Sprintf("new:p%d", u.placeholders)
	u.placeholders++
	u.cs.Mints = append(u.cs.Mints, &modelv1alpha1.Mint{Ref: ref, Kind: string(k.typ.kind), Rule: modelv1alpha1.MintRule_MINT_RULE_REFERENCE})
	u.mints[ref] = k.typ.kind
	if k.isID() {
		u.timelines[k.key] = &modelv1alpha1.BindingTimeline{Alias: string(k.key), Bindings: []*modelv1alpha1.Binding{{Alias: string(k.key), SubjectId: ref, Tentative: true}}}
		return nil
	}
	n, err := u.eng.name(ctx, k)
	if err != nil {
		return err
	}
	n.add(write{key: u.p.key, subject: ref, tentative: true})
	u.seeds = append(u.seeds, k)
	return nil
}

// finish computes the timelines, finds the placeholders the observed
// bindings replace, and assembles the ChangeSet.
func (u *run) finish(ctx context.Context) error {
	rows, err := u.settle(ctx)
	if err != nil {
		return err
	}
	changed, err := u.eng.changed(ctx, rows)
	if err != nil {
		return err
	}
	byAlias := map[model.Key]*modelv1alpha1.BindingTimeline{}
	for _, bt := range changed {
		byAlias[model.Key(bt.GetAlias())] = bt
	}
	for a, bt := range u.timelines {
		byAlias[a] = bt
	}
	for _, a := range slices.Sorted(keysOfTimelines(byAlias)) {
		u.cs.Bindings = append(u.cs.Bindings, byAlias[a])
	}
	u.cs.Merges = append(u.cs.Merges, u.merges...)
	entries, err := u.eng.stateEntries()
	if err != nil {
		return err
	}
	u.cs.State = append(u.cs.State, entries...)
	slices.SortFunc(u.cs.Mints, func(a, b *modelv1alpha1.Mint) int { return compareKeys(model.Key(a.GetRef()), model.Key(b.GetRef())) })
	return nil
}

// settle computes the timelines of every name the ChangeSet touches, and
// repeats while its own conclusions change them: an observed binding that
// replaces a tentative one merges the placeholder into the entity (rule
// placeholder), merged names compete for one subject, and a tentative
// binding that an observed one has replaced is forgotten.
func (u *run) settle(ctx context.Context) (map[model.Key][]*modelv1alpha1.Binding, error) {
	merged := false
	for {
		if err := u.seedPending(ctx); err != nil {
			return nil, err
		}
		rows, err := u.eng.rows(ctx, u.seeds)
		if err != nil {
			return nil, err
		}
		redo := false
		if !merged {
			merged = true
			for _, m := range u.placeholderMerges(ctx, rows) {
				u.merge(u.chosen, m, modelv1alpha1.MergeRule_MERGE_RULE_PLACEHOLDER)
				redo = true
			}
		}
		if u.dropReplacedTentatives(ctx, rows) {
			redo = true
		}
		if !redo {
			return rows, nil
		}
	}
}

// seedPending adds the names of subjects the ChangeSet merges to the seeds.
func (u *run) seedPending(ctx context.Context) error {
	for _, pair := range u.consolidate {
		if err := u.eng.consolidate(ctx, pair[0], pair[1]); err != nil {
			return err
		}
	}
	u.consolidate = nil
	for _, id := range u.pendingOwners {
		owned, err := u.g.owned(ctx, id)
		if err != nil {
			return err
		}
		for _, a := range owned {
			if k, ok := u.ix.lookup(string(a)); ok && k.isName() {
				u.seeds = append(u.seeds, k)
			}
		}
	}
	u.pendingOwners = nil
	return nil
}

// dropReplacedTentatives forgets the tentative writes whose binding no
// longer holds at the valid time a reference last resolved through them,
// and reports whether it forgot any. A placeholder's tentative binding is
// replaced, not kept for when the name is released again, so the outcome
// doesn't depend on whether the reference or the observation came first.
func (u *run) dropReplacedTentatives(ctx context.Context, rows map[model.Key][]*modelv1alpha1.Binding) bool {
	dropped := false
	for _, a := range slices.Sorted(keysOfNames(u.eng.names)) {
		n := u.eng.names[a]
		after, ok := rows[a]
		if !ok {
			continue
		}
		var kept []write
		for _, w := range n.writes {
			if w.tentative {
				b := covering(after, w.key.GetObservedAt().AsTime())
				if b == nil || !b.GetTentative() || u.g.mustCanon(ctx, b.GetSubjectId()) != u.g.mustCanon(ctx, w.subject) {
					dropped = true
					continue
				}
			}
			kept = append(kept, w)
		}
		if len(kept) != len(n.writes) {
			n.setWrites(kept)
		}
	}
	return dropped
}

func keysOfTimelines(m map[model.Key]*modelv1alpha1.BindingTimeline) func(func(model.Key) bool) {
	return func(yield func(model.Key) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// placeholderMerges returns the placeholders whose tentative binding of a
// name the entity's observed binding now replaces at a valid time a
// reference resolved the name: the tentative write's key carries the latest
// such time as its observed_at.
func (u *run) placeholderMerges(ctx context.Context, after map[model.Key][]*modelv1alpha1.Binding) []string {
	if u.p.obs.GetData().GetEntity().GetDeleted() {
		return nil
	}
	chosen := u.g.mustCanon(ctx, u.chosen)
	found := map[string]bool{}
	for _, k := range u.nameKeys() {
		n := u.eng.names[k.key]
		if n == nil {
			continue
		}
		for _, w := range n.tentativeBefore {
			p := u.g.mustCanon(ctx, w.subject)
			if p == chosen || found[p] {
				continue
			}
			at := w.key.GetObservedAt().AsTime()
			before := covering(u.g.rows[k.key], at)
			now := covering(after[k.key], at)
			if before != nil && before.GetTentative() && u.g.mustCanon(ctx, before.GetSubjectId()) == p &&
				now != nil && !now.GetTentative() && !now.GetReleased() && u.g.mustCanon(ctx, now.GetSubjectId()) == chosen {
				found[p] = true
			}
		}
	}
	return slices.Sorted(keysOf(found))
}
