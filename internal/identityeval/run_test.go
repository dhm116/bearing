package identityeval

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
)

// Floors for the evaluation run, set a little under the numbers of the
// default matcher (see docs/identity-evaluation.md) so a regression shows.
const (
	floorPrecision       = 0.70
	floorRecall          = 0.57
	floorCandidateRecall = 0.78
)

func openGraph(t testing.TB, now func() time.Time) *duckstore.Store {
	t.Helper()
	s, err := duckstore.Open(t.Context(), duckstore.Config{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ingest applies observations to g, one call per source.
func ingest(t testing.TB, g graph.Graph, obs []*eventv1alpha1.Observation) {
	t.Helper()
	bySource := map[string][]*eventv1alpha1.Observation{}
	for _, o := range obs {
		bySource[o.GetSource()] = append(bySource[o.GetSource()], o)
	}
	for src, os := range bySource {
		if _, err := g.Ingest(t.Context(), graph.IngestRequest{Source: src, Observations: os}); err != nil {
			t.Fatal(err)
		}
	}
}

// keyOf maps a record to the key of a labelled case it carries, else its ID,
// so a pair no case names counts as different.
func keyOf(r graph.Record, labelled map[model.Key]bool) model.Key {
	for _, k := range r.Keys {
		if labelled[k] {
			return k
		}
	}
	return model.Key(r.ID)
}

func labelledKeys(cases []Case) map[model.Key]bool {
	m := map[model.Key]bool{}
	for _, c := range cases {
		m[c.A], m[c.B] = true, true
	}
	return m
}

// openProposals lists the open proposed same_as links as pairs of labelled keys.
func openProposals(ctx context.Context, t testing.TB, g graph.Graph, cases []Case) []Pair {
	t.Helper()
	recs, err := g.Records(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	lab := labelledKeys(cases)
	key := map[graph.EntityID]model.Key{}
	for _, r := range recs {
		key[r.ID] = keyOf(r, lab)
	}
	ls, err := g.Links(ctx, graph.LinkFilter{Kind: graph.SameAs, Status: graph.Proposed})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]Pair, len(ls))
	for i, l := range ls {
		out[i] = NewPair(key[l.A], key[l.B])
	}
	return out
}

// candidatePairs runs the candidate pass over the graph's current records.
func candidatePairs(ctx context.Context, t testing.TB, g graph.Graph, cases []Case) []Pair {
	t.Helper()
	recs, err := g.Records(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	facts, err := g.Facts(ctx, graph.FactFilter{Predicate: "member_of"})
	if err != nil {
		t.Fatal(err)
	}
	members := map[graph.EntityID]map[graph.EntityID]bool{}
	for _, f := range facts {
		if f.Object == "" {
			continue
		}
		if members[f.Object] == nil {
			members[f.Object] = map[graph.EntityID]bool{}
		}
		members[f.Object][f.Subject] = true
	}
	lab := labelledKeys(cases)
	var out []Pair
	for _, c := range match.Candidates(recs, members, nil) {
		out = append(out, NewPair(keyOf(c.A, lab), keyOf(c.B, lab)))
	}
	return out
}

func TestEvalGeneratedSet(t *testing.T) {
	t.Parallel()
	set := Generate(1, t0)
	g := openGraph(t, func() time.Time { return t0 })
	ingest(t, g, set.Observations)
	ctx := t.Context()

	cands := candidatePairs(ctx, t, g, set.Cases)
	if _, err := match.Run(ctx, g, match.Config{Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	proposed := openProposals(ctx, t, g, set.Cases)

	groups := map[string]bool{}
	for _, c := range set.Cases {
		groups[c.Group] = true
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("%-18s %6s %6s %9s %9s %9s", "group", "cases", "same", "precision", "recall", "cand.rec")
	row := func(name string, cs []Case, ps, cp []Pair) {
		p, r := PrecisionRecall(cs, ps)
		same := 0
		for _, c := range cs {
			if c.Same {
				same++
			}
		}
		// With no same cases there is nothing to recall: show "-".
		rc, cr := "-", "-"
		if same > 0 {
			rc, cr = fmt.Sprintf("%.3f", r), fmt.Sprintf("%.3f", CandidateRecall(cs, cp))
		}
		t.Logf("%-18s %6d %6d %9.3f %9s %9s", name, len(cs), same, p, rc, cr)
	}
	for _, n := range names {
		var cs []Case
		in := map[Pair]bool{}
		for _, c := range set.Cases {
			if c.Group == n {
				cs = append(cs, c)
				in[c.Pair()] = true
			}
		}
		keep := func(ps []Pair) []Pair {
			return slices.DeleteFunc(slices.Clone(ps), func(p Pair) bool { return !in[NewPair(p.A, p.B)] })
		}
		row(n, cs, keep(proposed), keep(cands))
	}
	row("all", set.Cases, proposed, cands)
	labelled := map[Pair]bool{}
	for _, c := range set.Cases {
		labelled[c.Pair()] = true
	}
	unlabelled := 0
	for _, p := range proposed {
		if !labelled[NewPair(p.A, p.B)] {
			unlabelled++
		}
	}
	t.Logf("%d proposals open, %d for pairs no case names (all wrong)", len(proposed), unlabelled)

	p, r := PrecisionRecall(set.Cases, proposed)
	if cr := CandidateRecall(set.Cases, cands); cr < floorCandidateRecall {
		t.Errorf("candidate recall %.3f, want at least %.2f", cr, floorCandidateRecall)
	}
	if p < floorPrecision {
		t.Errorf("precision %.3f, want at least %.2f", p, floorPrecision)
	}
	if r < floorRecall {
		t.Errorf("recall %.3f, want at least %.2f", r, floorRecall)
	}
}
