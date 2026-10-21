package match

import (
	"context"
	"strconv"
	"testing"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// fake is an in-memory Graph: it opens a link per new unordered pair, adds a
// deposit to a known one and closes links on request. It does not cluster.
type fake struct {
	recs  []graph.Record
	facts []graph.Fact
	links []graph.Link
}

func (f *fake) Records(_ context.Context, k model.Kind) ([]graph.Record, error) {
	var out []graph.Record
	for _, r := range f.recs {
		if k == "" || r.Kind == k {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fake) Facts(context.Context, graph.FactFilter) ([]graph.Fact, error) { return f.facts, nil }

func (f *fake) Links(_ context.Context, lf graph.LinkFilter) ([]graph.Link, error) {
	var out []graph.Link
	for _, l := range f.links {
		if (lf.Kind == "" || l.Kind == lf.Kind) && (lf.Status == "" || l.Status == lf.Status) &&
			(lf.WithClosed || l.ValidTo.IsZero()) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fake) Link(_ context.Context, ps []graph.Proposal) (graph.LinkResult, error) {
	var res graph.LinkResult
	for _, p := range ps {
		found := false
		for i, l := range f.links {
			if l.ValidTo.IsZero() && l.Kind == p.Kind && pairOf(l.A, l.B) == pairOf(p.A, p.B) {
				f.links[i].Deposits = append(l.Deposits, p.Deposit)
				if p.Status == graph.Asserted {
					f.links[i].Status = graph.Asserted
				}
				res.Reinforced++
				found = true
			}
		}
		if !found {
			f.links = append(f.links, graph.Link{
				ID: "L" + strconv.Itoa(len(f.links)+1), A: p.A, B: p.B, Kind: p.Kind, Status: p.Status,
				Rule: p.Rule, Deposits: []graph.Deposit{p.Deposit}, ValidFrom: p.Deposit.At,
			})
			res.Opened++
		}
	}
	return res, nil
}

func (f *fake) CloseLink(_ context.Context, id string, at time.Time) error {
	for i, l := range f.links {
		if l.ID == id {
			f.links[i].ValidTo = at
		}
	}
	return nil
}

// teamFacts gives a record's cluster one fact observed at t.
func teamFacts(id string, at time.Time) graph.Fact {
	return graph.Fact{
		Subject: graph.EntityID(id), Predicate: "name", Value: id,
		Sources: []graph.Source{{ObservedAt: at}},
	}
}

func newFake() *fake {
	return &fake{
		recs: []graph.Record{
			rec("g-pay", "payments", model.KindTeam, "github:team/acme/payments"),
			rec("d-pay", "Payments", model.KindTeam, "authentik:group/payments"),
			rec("k-pay", "pay", model.KindTeam, "kubernetes:team/payments"),
			rec("g-sre", "sre", model.KindTeam),
		},
		facts: []graph.Fact{teamFacts("g-pay", t0), teamFacts("d-pay", t0.Add(day)), teamFacts("g-sre", t0)},
	}
}

func TestRunProposesAndRerunAddsNothing(t *testing.T) {
	g, now := newFake(), t0.Add(2*day)
	cfg := Config{Now: func() time.Time { return now }}

	res, err := Run(context.Background(), g, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Proposed != 1 || len(g.links) != 1 {
		t.Fatalf("got %d proposed, %d links; want 1, 1", res.Proposed, len(g.links))
	}
	l := g.links[0]
	if l.Rule != TransparentRule || l.Status != graph.Proposed || l.Kind != graph.SameAs {
		t.Fatalf("got %+v", l)
	}
	d := l.Deposits[0]
	if !d.At.Equal(t0.Add(day)) || d.Features["name"] != 1 || d.Features["score"] != 0.6 {
		t.Fatalf("got deposit %+v, want At newest evidence and name feature 1", d)
	}
	if len(res.Review) != 1 || res.Review[0].Link.ID != l.ID {
		t.Fatalf("got review %+v, want the proposal", res.Review)
	}

	now = now.Add(day) // the clock moves, the evidence does not
	res, _ = Run(context.Background(), g, cfg)
	if res.Proposed != 0 || len(g.links[0].Deposits) != 1 {
		t.Fatalf("rerun added a deposit: %+v", g.links[0].Deposits)
	}
}

func TestRunNewEvidenceReinforces(t *testing.T) {
	g, now := newFake(), t0.Add(2*day)
	cfg := Config{Now: func() time.Time { return now }}
	if _, err := Run(context.Background(), g, cfg); err != nil {
		t.Fatal(err)
	}
	g.facts = append(g.facts, teamFacts("g-pay", t0.Add(5*day)))
	now = t0.Add(6 * day)
	res, _ := Run(context.Background(), g, cfg)
	if res.Proposed != 1 || len(g.links) != 1 || len(g.links[0].Deposits) != 2 {
		t.Fatalf("got %d links, deposits %+v; want one link with two deposits", len(g.links), g.links[0].Deposits)
	}
}

func TestRunStaleProposalDecaysBelowFloor(t *testing.T) {
	g, now := newFake(), t0.Add(2*day)
	cfg := Config{Now: func() time.Time { return now }}
	if _, err := Run(context.Background(), g, cfg); err != nil {
		t.Fatal(err)
	}
	// strength 0.6/1.6 = 0.375; one half-life later 0.3/1.3 = 0.23: still open.
	now = t0.Add(day + 30*day)
	res, _ := Run(context.Background(), g, cfg)
	if res.Closed != 0 || len(res.Review) != 0 {
		t.Fatalf("got closed %d, review %d after one half-life; want 0, 0", res.Closed, len(res.Review))
	}
	// 0.6 x 2^-3 = 0.075; strength 0.07 < 0.1.
	now = t0.Add(day + 90*day)
	res, _ = Run(context.Background(), g, cfg)
	if res.Closed != 1 || g.links[0].ValidTo.IsZero() {
		t.Fatalf("got closed %d, link %+v; want the proposal closed", res.Closed, g.links[0])
	}
	// The same evidence does not bring it back.
	now = now.Add(day)
	res, _ = Run(context.Background(), g, cfg)
	if res.Proposed != 0 || len(g.links) != 1 {
		t.Fatalf("closed proposal came back: %+v", g.links)
	}
}

func TestRunHalfLifeByKind(t *testing.T) {
	g, now := newFake(), t0.Add(2*day)
	cfg := Config{Now: func() time.Time { return now }, HalfLife: map[model.Kind]time.Duration{model.KindTeam: 2 * day}}
	if _, err := Run(context.Background(), g, cfg); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(day + 10*day) // five short half-lives
	res, _ := Run(context.Background(), g, cfg)
	if res.Closed != 1 {
		t.Fatalf("got closed %d, want 1", res.Closed)
	}
}

func TestRunSkipsDistinctAndJoinedPairs(t *testing.T) {
	for _, tc := range []struct {
		name string
		link graph.Link
	}{
		{"distinct_from", graph.Link{ID: "X", A: "d-pay", B: "g-pay", Kind: graph.DistinctFrom, Status: graph.Asserted}},
		{"asserted same_as", graph.Link{ID: "X", A: "g-pay", B: "d-pay", Kind: graph.SameAs, Status: graph.Asserted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFake()
			g.links = []graph.Link{tc.link}
			res, _ := Run(context.Background(), g, Config{Now: func() time.Time { return t0.Add(2 * day) }})
			if res.Proposed != 0 || len(g.links) != 1 {
				t.Fatalf("got %d proposed, links %+v; want none", res.Proposed, g.links)
			}
		})
	}
}

func TestRunRulesAssertLinks(t *testing.T) {
	g := newFake()
	cfg := Config{
		Now:   func() time.Time { return t0.Add(2 * day) },
		Rules: []KeyRule{{Name: "k8s-team", A: "kubernetes:team/{name}", B: "github:team/acme/{name}"}},
	}
	res, err := Run(context.Background(), g, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var found *graph.Link
	for i, l := range g.links {
		if l.Rule == "rule/k8s-team" {
			found = &g.links[i]
		}
	}
	if res.Asserted != 1 || found == nil || found.Status != graph.Asserted ||
		pairOf(found.A, found.B) != pairOf("g-pay", "k-pay") {
		t.Fatalf("got asserted %d, links %+v", res.Asserted, g.links)
	}
	res, _ = Run(context.Background(), g, cfg)
	if res.Asserted != 0 {
		t.Fatalf("rerun asserted %d, want 0", res.Asserted)
	}
}

func TestRunRejectsBadRule(t *testing.T) {
	if _, err := Run(context.Background(), newFake(), Config{Rules: []KeyRule{{Name: "bad"}}}); err == nil {
		t.Fatal("bad rule accepted")
	}
}

func TestRunMatchesPeopleByEmailAndMembers(t *testing.T) {
	g := &fake{
		recs: []graph.Record{
			rec("gh-jane", "jane-d", model.KindPerson),
			rec("dir-jane", "Jane Doe", model.KindPerson),
			rec("t1", "alpha", model.KindTeam),
			rec("t2", "beta", model.KindTeam),
		},
		facts: []graph.Fact{
			{Subject: "gh-jane", Predicate: "email", Value: "Jane@acme.test", Sources: []graph.Source{{ObservedAt: t0}}},
			{Subject: "dir-jane", Predicate: "email", Value: "jane@acme.test", Sources: []graph.Source{{ObservedAt: t0}}},
		},
	}
	// Two different teams sharing their only member: name 0.x*0.6 + 0.3.
	g.facts = append(g.facts,
		graph.Fact{Subject: "gh-jane", Predicate: "member_of", Object: "t1", Sources: []graph.Source{{ObservedAt: t0}}},
		graph.Fact{Subject: "gh-jane", Predicate: "member_of", Object: "t2", Sources: []graph.Source{{ObservedAt: t0}}})

	res, _ := Run(context.Background(), g, Config{Now: func() time.Time { return t0 }})
	if res.Proposed != 1 {
		t.Fatalf("got %d proposed (%+v); want only the people pair", res.Proposed, g.links)
	}
	d := g.links[0].Deposits[0]
	if d.Features["email"] != 1 || pairOf(g.links[0].A, g.links[0].B) != pairOf("dir-jane", "gh-jane") {
		t.Fatalf("got %+v", g.links[0])
	}
	if d.Weight < 0.4 {
		t.Fatalf("got weight %v, want at least the email weight", d.Weight)
	}
}

func TestRunReviewOrderedStrongestFirst(t *testing.T) {
	g := newFake()
	now := t0.Add(2 * day)
	// A strong link: many fresh deposits.
	g.links = []graph.Link{{
		ID: "S", A: "g-sre", B: "k-pay", Kind: graph.SameAs, Status: graph.Proposed,
		Deposits: []graph.Deposit{{At: now, Weight: 1}, {At: now, Weight: 1}},
	}}
	res, _ := Run(context.Background(), g, Config{Now: func() time.Time { return now }})
	if len(res.Review) != 2 || res.Review[0].Link.ID != "S" ||
		res.Review[0].Score.Strength <= res.Review[1].Score.Strength {
		t.Fatalf("got %+v", res.Review)
	}
}
