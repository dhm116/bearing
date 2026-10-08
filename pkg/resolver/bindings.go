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

// nameState is a name alias's writes as loaded from the resolver's state,
// plus what this ChangeSet changes in them.
type nameState struct {
	nameWrites
	// loaded holds the subjects whose state entry exists.
	loaded map[string]bool
	// dirty holds the subjects whose entry this ChangeSet rewrites.
	dirty map[string]bool
	// tentativeBefore holds the tentative writes as loaded: the placeholders
	// references made for the name before this ChangeSet.
	tentativeBefore []write
	// hadObserved is true if the name had an observed write before.
	hadObserved bool
}

// engine computes alias binding timelines from the remembered writes. It
// loads each name it touches once, applies the ChangeSet's writes, and
// recomputes the timelines of every name that competes with them.
type engine struct {
	g     *graph
	ix    *index
	names map[model.Key]*nameState
	// marks holds the deletion marks read or written, by namespace and
	// canonical subject.
	marks map[markID]*markState
}

// markID names the deletion marks of a subject in a namespace.
type markID struct{ ns, subject string }

type markState struct {
	list    []mark
	existed bool // the state entry exists
	dirty   bool
}

func newEngine(g *graph, ix *index) *engine {
	return &engine{g: g, ix: ix, names: map[model.Key]*nameState{}, marks: map[markID]*markState{}}
}

// markState loads the deletion marks of a subject in a namespace.
func (e *engine) markState(ctx context.Context, ns, subject string) (*markState, error) {
	id := markID{ns, subject}
	if m, ok := e.marks[id]; ok {
		return m, nil
	}
	m := &markState{}
	if !isRef(subject) {
		got, err := e.g.states(ctx, delKey(ns, subject))
		if err != nil {
			return nil, err
		}
		if a, ok := got[delKey(ns, subject)]; ok {
			if m.list, err = unpackMarks(a); err != nil {
				return nil, fmt.Errorf("%s: %w", delKey(ns, subject), err)
			}
			m.existed = true
		}
	}
	e.marks[id] = m
	return m, nil
}

// addMark records that the subject, canonically, was deleted in the
// namespace at m.at with m.key.
func (e *engine) addMark(ctx context.Context, ns, subject string, m mark) error {
	c, err := e.g.canon(ctx, subject)
	if err != nil {
		return err
	}
	st, err := e.markState(ctx, ns, c)
	if err != nil {
		return err
	}
	if next, changed := addMark(st.list, m); changed {
		st.list, st.dirty = next, true
	}
	return nil
}

// consolidate moves the deletion marks of a subject the ChangeSet merges
// into its survivor, so marks always sit under the canonical subject.
func (e *engine) consolidate(ctx context.Context, survivor, merged string) error {
	for _, ns := range slices.Sorted(maps.Keys(e.ix.namespaces)) {
		from, err := e.markState(ctx, ns, merged)
		if err != nil {
			return err
		}
		if len(from.list) == 0 && !from.existed {
			continue
		}
		to, err := e.markState(ctx, ns, survivor)
		if err != nil {
			return err
		}
		for _, m := range from.list {
			to.list, _ = addMark(to.list, m)
		}
		to.dirty = true
		from.list, from.dirty = nil, from.existed
	}
	return nil
}

// preload reads the deletion marks of the subjects the names write to, as
// canonical now, and returns the lookup bindingRows needs.
func (e *engine) preload(ctx context.Context, names []*nameState) (marksFor, error) {
	for _, n := range names {
		for _, w := range n.writes {
			if w.tentative {
				continue
			}
			c, err := e.g.canon(ctx, w.subject)
			if err != nil {
				return nil, err
			}
			if _, err := e.markState(ctx, n.ns, c); err != nil {
				return nil, err
			}
		}
	}
	return func(ns, subject string) []mark {
		if m, ok := e.marks[markID{ns, e.g.mustCanon(ctx, subject)}]; ok {
			return m.list
		}
		return nil
	}, nil
}

// name loads the writes of a name alias.
func (e *engine) name(ctx context.Context, k keyRef) (*nameState, error) {
	if n, ok := e.names[k.key]; ok {
		return n, nil
	}
	rows, err := e.g.timeline(ctx, k.key)
	if err != nil {
		return nil, err
	}
	subjects := map[string]bool{}
	for _, b := range rows {
		if b.GetSubjectId() != "" {
			subjects[b.GetSubjectId()] = true
		}
	}
	bySubject := map[string]string{} // state key to subject
	var keys []string
	for _, s := range slices.Sorted(maps.Keys(subjects)) {
		key := bindKey(k.key, s)
		bySubject[key] = s
		keys = append(keys, key)
	}
	entries, err := e.g.states(ctx, keys...)
	if err != nil {
		return nil, err
	}
	n := &nameState{
		nameWrites: nameWrites{alias: k.key, ns: k.ns, perSubject: k.typ.perSubject, redirects: k.typ.redirects},
		loaded:     map[string]bool{}, dirty: map[string]bool{},
	}
	for _, key := range keys {
		a, ok := entries[key]
		if !ok {
			continue
		}
		ws, err := unpackWrites(a, bySubject[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		n.loaded[bySubject[key]] = true
		n.writes = append(n.writes, ws...)
	}
	for _, w := range n.writes {
		if !w.tentative {
			n.hadObserved = true
		}
	}
	// A tentative binding to a placeholder that has since merged into
	// another subject has done its job.
	var kept []write
	for _, w := range n.writes {
		if w.tentative && e.g.mustCanon(ctx, w.subject) != w.subject {
			n.dirty[w.subject] = true
			continue
		}
		kept = append(kept, w)
	}
	n.writes = kept
	for _, w := range n.writes {
		if w.tentative {
			n.tentativeBefore = append(n.tentativeBefore, w)
		}
	}
	e.names[k.key] = n
	return n, nil
}

// add remembers a write and marks the entries it changes.
func (n *nameState) add(w write) {
	next, changed := addWrite(n.writes, w)
	if !changed {
		return
	}
	n.setWrites(next)
}

// setWrites replaces the name's writes, marking every entry that differs.
func (n *nameState) setWrites(next []write) {
	for _, s := range subjectsOf(n.writes, next) {
		if !slices.EqualFunc(writesTo(n.writes, s), writesTo(next, s), sameWrite) {
			n.dirty[s] = true
		}
	}
	n.writes = next
}

func sameWrite(a, b write) bool {
	return a.tentative == b.tentative && a.subject == b.subject && a.from.Equal(b.from) &&
		model.CompareOrderingKeys(a.key, b.key) == 0
}

func subjectsOf(a, b []write) []string {
	seen := map[string]bool{}
	var out []string
	for _, ws := range [][]write{a, b} {
		for _, w := range ws {
			if !seen[w.subject] {
				seen[w.subject] = true
				out = append(out, w.subject)
			}
		}
	}
	return out
}

// writesTo returns the writes under one state entry, in a fixed order.
func writesTo(ws []write, subject string) []write {
	var out []write
	for _, w := range ws {
		if w.subject == subject {
			out = append(out, w)
		}
	}
	slices.SortStableFunc(out, writeCmp)
	return out
}

// entries returns the state entries this ChangeSet rewrites for the name.
func (n *nameState) entries() ([]*modelv1alpha1.StateEntry, error) {
	var out []*modelv1alpha1.StateEntry
	for _, s := range slices.Sorted(maps.Keys(n.dirty)) {
		ws := writesTo(n.writes, s)
		if len(ws) == 0 && !n.loaded[s] {
			continue
		}
		e, err := entryFor(n.alias, s, ws)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// closure returns the names whose timelines can change when the seeds'
// writes do: for a per_subject: one key type, every name that has a write to
// a subject a member of the closure writes to.
func (e *engine) closure(ctx context.Context, seeds []keyRef) ([]*nameState, error) {
	in := map[model.Key]bool{}
	var out []*nameState
	var queue []keyRef
	visited := map[string]bool{}
	for _, s := range seeds {
		if !in[s.key] {
			in[s.key] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		n, err := e.name(ctx, k)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
		if !k.typ.perSubject {
			continue
		}
		for _, w := range n.writes {
			if w.tentative {
				continue
			}
			c, err := e.g.canon(ctx, w.subject)
			if err != nil {
				return nil, err
			}
			if visited[k.groupKey()+"|"+c] {
				continue
			}
			visited[k.groupKey()+"|"+c] = true
			owners, err := e.g.owned(ctx, c)
			if err != nil {
				return nil, err
			}
			for _, o := range owners {
				kr, found := e.ix.lookup(string(o))
				if found && kr.isName() && kr.groupKey() == k.groupKey() && !in[kr.key] {
					in[kr.key] = true
					queue = append(queue, kr)
				}
			}
			// Names this ChangeSet has already loaded, which may write to a
			// subject it mints.
			for _, other := range slices.Sorted(maps.Keys(e.names)) {
				on := e.names[other]
				if in[other] || on.perSubject != n.perSubject {
					continue
				}
				if kr, found := e.ix.lookup(string(other)); !found || kr.groupKey() != k.groupKey() {
					continue
				}
				for _, ow := range on.writes {
					if !ow.tentative && e.g.mustCanon(ctx, ow.subject) == c {
						in[other] = true
						kr, _ := e.ix.lookup(string(other))
						queue = append(queue, kr)
						break
					}
				}
			}
		}
	}
	return out, nil
}

// rows computes the timelines of every name the seeds' closures hold and
// returns them by alias. Names of different families compete separately.
func (e *engine) rows(ctx context.Context, seeds []keyRef) (map[model.Key][]*modelv1alpha1.Binding, error) {
	families := map[string][]keyRef{}
	var order []string
	for _, s := range seeds {
		g := s.groupKey()
		if _, ok := families[g]; !ok {
			order = append(order, g)
		}
		families[g] = append(families[g], s)
	}
	slices.Sort(order)
	canon := func(id string) string { return e.g.mustCanon(ctx, id) }
	out := map[model.Key][]*modelv1alpha1.Binding{}
	for _, f := range order {
		members, err := e.closure(ctx, families[f])
		if err != nil {
			return nil, err
		}
		marks, err := e.preload(ctx, members)
		if err != nil {
			return nil, err
		}
		ws := make([]*nameWrites, len(members))
		for i, m := range members {
			ws[i] = &m.nameWrites
		}
		for a, rows := range bindingRows(ws, canon, marks) {
			sortRows(rows)
			out[a] = rows
		}
	}
	return out, nil
}

// changed returns, in alias order, the timelines in rows that differ from
// the store's current ones.
func (e *engine) changed(ctx context.Context, rows map[model.Key][]*modelv1alpha1.Binding) ([]*modelv1alpha1.BindingTimeline, error) {
	var out []*modelv1alpha1.BindingTimeline
	for _, a := range slices.Sorted(maps.Keys(rows)) {
		cur, err := e.g.timeline(ctx, a)
		if err != nil {
			return nil, err
		}
		if !sameRows(cur, rows[a]) {
			out = append(out, &modelv1alpha1.BindingTimeline{Alias: string(a), Bindings: rows[a]})
		}
	}
	return out, nil
}

// stateEntries returns the entries of every name and deletion mark the
// ChangeSet changed.
func (e *engine) stateEntries() ([]*modelv1alpha1.StateEntry, error) {
	var out []*modelv1alpha1.StateEntry
	for _, a := range slices.Sorted(maps.Keys(e.names)) {
		es, err := e.names[a].entries()
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	ids := slices.SortedFunc(func(yield func(markID) bool) {
		for id := range e.marks {
			if !yield(id) {
				return
			}
		}
	}, func(a, b markID) int {
		if a.ns != b.ns {
			return cmp.Compare(a.ns, b.ns)
		}
		return cmp.Compare(a.subject, b.subject)
	})
	for _, id := range ids {
		m := e.marks[id]
		if !m.dirty {
			continue
		}
		entry := &modelv1alpha1.StateEntry{Key: delKey(id.ns, id.subject)}
		if len(m.list) > 0 {
			v, err := packMarks(m.list)
			if err != nil {
				return nil, err
			}
			entry.Value = v
		} else if !m.existed {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}
