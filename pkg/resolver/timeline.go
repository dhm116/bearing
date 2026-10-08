package resolver

import (
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// write is one remembered write to a name alias: an observed binding to a
// subject from `from` on, or a tentative binding a reference made to a
// placeholder (docs/spec/data-model.md, "Bindings").
type write struct {
	key *resolverv1alpha1.OrderingKey
	// from is where an observed write starts, extending to infinity. A
	// tentative write has no start.
	from time.Time
	// subject is the subject as written.
	subject   string
	tentative bool
}

// nameWrites is every write to one name alias.
type nameWrites struct {
	alias model.Key
	// ns is the alias's namespace, whose deletion marks apply to it.
	ns string
	// perSubject and redirects are the alias's key type's options.
	perSubject, redirects bool
	writes                []write
}

// mark is a deletion: from valid time `at` on, with `key`, the names of a
// subject in a namespace are released unless a write with a greater key
// covers them (docs/spec/data-model.md, "Normalization": entity.deleted
// releases the entity's names). It is a watermark of the subject, so it ends
// names bound to the subject by writes applied later, too.
type mark struct {
	at  time.Time
	key *resolverv1alpha1.OrderingKey
}

// addMark adds m to ms, dropping a mark another makes irrelevant.
func addMark(ms []mark, m mark) ([]mark, bool) {
	for _, o := range ms {
		if !o.at.After(m.at) && model.CompareOrderingKeys(o.key, m.key) >= 0 {
			return ms, false
		}
	}
	out := make([]mark, 0, len(ms)+1)
	for _, o := range ms {
		if !m.at.After(o.at) && model.CompareOrderingKeys(m.key, o.key) > 0 {
			continue
		}
		out = append(out, o)
	}
	return append(out, m), true
}

// addWrite adds w to ws and drops the writes it makes irrelevant: an
// observed write is irrelevant when another starts no later and has a
// greater key, since the greater key wins at every valid time the other
// covers. A tentative write replaces the tentative write to the same subject
// when its key is greater. It returns the new list and whether it changed.
func addWrite(ws []write, w write) ([]write, bool) {
	if w.tentative {
		for i, o := range ws {
			if o.tentative && o.subject == w.subject {
				if model.CompareOrderingKeys(w.key, o.key) <= 0 {
					return ws, false
				}
				out := slices.Clone(ws)
				out[i] = w
				return out, true
			}
		}
		return append(slices.Clone(ws), w), true
	}
	for _, o := range ws {
		if o.tentative {
			continue
		}
		c := model.CompareOrderingKeys(o.key, w.key)
		if !o.from.After(w.from) && (c > 0 || c == 0 && o.from.Equal(w.from) && o.subject == w.subject) {
			return ws, false
		}
	}
	out := make([]write, 0, len(ws)+1)
	for _, o := range ws {
		if !o.tentative && !w.from.After(o.from) && model.CompareOrderingKeys(w.key, o.key) > 0 {
			continue
		}
		out = append(out, o)
	}
	return append(out, w), true
}

// cell is one name's mapping over one stretch of valid time, before the
// per-subject rule and tentative bindings are applied.
type cell struct {
	kind    cellKind
	subject string // as written; for a release, the subject released from
	key     *resolverv1alpha1.OrderingKey
}

type cellKind int

const (
	// cellSubj: a covering observed write binds the name to subject.
	cellSubj cellKind = iota + 1
	// cellBack: before the name's earliest write, which binds it to subject.
	cellBack
	// cellReleased: released, because the subject was deleted or holds
	// another name; subject is the subject it was released from.
	cellReleased
)

// marksFor returns the deletion marks of a subject, given as written, in the
// name's namespace.
type marksFor func(ns, subject string) []mark

// cells computes the name's cell for each of the segments the breakpoints
// cut valid time into: segment 0 is before bp[0], segment i is [bp[i-1],
// bp[i]), and the last runs to infinity.
func (n *nameWrites) cells(bp []time.Time, marks marksFor) []cell {
	var obs []write
	for _, w := range n.writes {
		if !w.tentative {
			obs = append(obs, w)
		}
	}
	slices.SortStableFunc(obs, func(a, b write) int { return a.from.Compare(b.from) })
	out := make([]cell, len(bp)+1)
	if len(obs) == 0 {
		return out
	}
	// The write that wins at the earliest start decides the back-extended
	// mapping before it.
	first := obs[0]
	for _, w := range obs {
		if w.from.Equal(obs[0].from) && model.CompareOrderingKeys(w.key, first.key) > 0 {
			first = w
		}
	}
	var best *write
	p := 0
	for seg := range out {
		if seg == 0 {
			out[seg] = cell{kind: cellBack, subject: first.subject, key: first.key}
			continue
		}
		start := bp[seg-1]
		for p < len(obs) && !obs[p].from.After(start) {
			if best == nil || model.CompareOrderingKeys(obs[p].key, best.key) > 0 {
				best = &obs[p]
			}
			p++
		}
		if best == nil {
			out[seg] = cell{kind: cellBack, subject: first.subject, key: first.key}
			continue
		}
		out[seg] = cell{kind: cellSubj, subject: best.subject, key: best.key}
		for _, m := range marks(n.ns, best.subject) {
			if !m.at.After(start) && model.CompareOrderingKeys(m.key, best.key) > 0 {
				out[seg] = cell{kind: cellReleased, subject: best.subject, key: m.key}
				break
			}
		}
	}
	return out
}

// bestTentative returns the name's tentative write with the greatest key.
func (n *nameWrites) bestTentative() *write {
	var best *write
	for i, w := range n.writes {
		if w.tentative && (best == nil || model.CompareOrderingKeys(w.key, best.key) > 0 ||
			model.CompareOrderingKeys(w.key, best.key) == 0 && w.subject < best.subject) {
			best = &n.writes[i]
		}
	}
	return best
}

// bindingRows computes the binding timelines of a family of names (one
// namespace and key type) from their writes. canon maps a subject as
// written to its canonical subject, after the merges known so far, and marks
// gives a subject's deletion marks.
//
// At each valid time a name maps to the subject of its covering write with
// the greatest key, unless a deletion of that subject with a greater key
// covers it. Before its earliest write it maps to that write's subject
// (back-extension). For a per_subject: one key type, a subject holds only
// the name whose covering write to it has the greatest key; the others are
// released, and a back-extended mapping is dropped where the subject has a
// holder. Tentative bindings fill what is left unbound, or released without
// a redirect.
func bindingRows(names []*nameWrites, canon func(string) string, marks marksFor) map[model.Key][]*modelv1alpha1.Binding {
	var bp []time.Time
	for _, n := range names {
		for _, w := range n.writes {
			if w.tentative {
				continue
			}
			bp = append(bp, w.from)
			for _, m := range marks(n.ns, w.subject) {
				bp = append(bp, m.at)
			}
		}
	}
	slices.SortFunc(bp, time.Time.Compare)
	bp = dedupTimes(bp)
	cells := make([][]cell, len(names))
	for i, n := range names {
		cells[i] = n.cells(bp, marks)
	}
	for seg := 0; seg <= len(bp); seg++ {
		holder := map[string]int{}
		for i, n := range names {
			if c := cells[i][seg]; n.perSubject && c.kind == cellSubj {
				s := canon(c.subject)
				if h, ok := holder[s]; !ok || outranks(c, n.alias, cells[h][seg], names[h].alias) {
					holder[s] = i
				}
			}
		}
		for i, n := range names {
			c := cells[i][seg]
			if !n.perSubject {
				continue
			}
			switch c.kind {
			case cellSubj:
				if holder[canon(c.subject)] != i {
					cells[i][seg].kind = cellReleased
				}
			case cellBack:
				if _, held := holder[canon(c.subject)]; held {
					cells[i][seg] = cell{}
				}
			}
		}
	}
	out := make(map[model.Key][]*modelv1alpha1.Binding, len(names))
	for i, n := range names {
		out[n.alias] = n.rows(cells[i], bp)
	}
	return out
}

// outranks reports whether cell a of name x beats cell b of name y for
// holding a subject: the greater key, then the lower alias.
func outranks(a cell, x model.Key, b cell, y model.Key) bool {
	if c := model.CompareOrderingKeys(a.key, b.key); c != 0 {
		return c > 0
	}
	return x < y
}

func dedupTimes(ts []time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if len(out) == 0 || !out[len(out)-1].Equal(t) {
			out = append(out, t)
		}
	}
	return out
}

// row is one stretch of a name's final mapping.
type row struct {
	released  bool
	bound     bool
	subject   string
	tentative bool
}

// rows turns the name's cells into binding rows, joining neighbours that
// map alike.
func (n *nameWrites) rows(cells []cell, bp []time.Time) []*modelv1alpha1.Binding {
	tent := n.bestTentative()
	segs := make([]row, len(cells))
	for i, c := range cells {
		switch c.kind {
		case cellSubj, cellBack:
			segs[i] = row{bound: true, subject: c.subject}
		case cellReleased:
			segs[i] = row{bound: true, released: true}
			if n.redirects {
				segs[i].subject = c.subject
			}
		}
		if tent != nil && (!segs[i].bound || segs[i].released && segs[i].subject == "") {
			segs[i] = row{bound: true, subject: tent.subject, tentative: true}
		}
	}
	var out []*modelv1alpha1.Binding
	for i := 0; i < len(segs); {
		j := i + 1
		for j < len(segs) && segs[j] == segs[i] {
			j++
		}
		if segs[i].bound {
			b := &modelv1alpha1.Binding{
				Alias: string(n.alias), SubjectId: segs[i].subject,
				Released: segs[i].released, Tentative: segs[i].tentative,
			}
			if i > 0 {
				b.ValidFrom = timestamppb.New(bp[i-1])
			}
			if j <= len(bp) {
				b.ValidTo = timestamppb.New(bp[j-1])
			}
			out = append(out, b)
		}
		i = j
	}
	return out
}
