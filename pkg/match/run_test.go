package match

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// fake is an in-memory IdentityGraph: it opens a link per new unordered pair, adds a
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

// seen sets when a source last observed the records.
func (f *fake) seen(at time.Time, ids ...graph.EntityID) {
	for i, r := range f.recs {
		if slices.Contains(ids, r.ID) {
			f.recs[i].LastSeen = at
		}
	}
}

func newFake() *fake {
	f := &fake{
		recs: []graph.Record{
			rec("g-pay", "payments", model.KindTeam, "github:team/acme/payments"),
			rec("d-pay", "Payments", model.KindTeam, "authentik:group/payments"),
			rec("k-pay", "pay", model.KindTeam, "kubernetes:team/payments"),
			rec("g-sre", "sre", model.KindTeam),
		},
	}
	f.seen(t0, "g-pay", "g-sre")
	f.seen(t0.Add(day), "d-pay")
	return f
}

func TestRunProposesAndRerunAddsNothing(t *testing.T) {
	g, now := newFake(), t0.Add(2*day)
	cfg := Config{Now: func() time.Time { return now }}

	res, err := Run(t.Context(), g, cfg)
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
	if !d.At.Equal(t0) || d.Features["name"] != 1 || d.Features["score"] != 0.6 {
		t.Fatalf("got deposit %+v, want At the earlier side's evidence (t0) and name feature 1", d)
	}
	if len(res.Review) != 1 || res.Review[0].Link.ID != l.ID {
		t.Fatalf("got review %+v, want the proposal", res.Review)
	}

	now = now.Add(day) // the clock moves, the evidence does not
	if res, err = Run(t.Context(), g, cfg); err != nil {
		t.Fatal(err)
	}
	if res.Proposed != 0 || len(g.links[0].Deposits) != 1 {
		t.Fatalf("rerun added a deposit: %+v", g.links[0].Deposits)
	}
}

func TestRunDepositsOnlyForNewObservationsThatChangeSomethingOrAHalfLifeLater(t *testing.T) {
	member := func(at time.Time, team graph.EntityID) graph.Fact {
		return graph.Fact{Subject: "p1", Predicate: "member_of", Object: team, Sources: []graph.Source{{ObservedAt: at}}}
	}
	for _, tt := range []struct {
		name   string
		evolve func(g *fake)
		want   int // deposits after the second run
	}{
		{"same features, a day newer", func(g *fake) { g.seen(t0.Add(3*day), "g-pay", "d-pay") }, 1},
		{"one side newer only", func(g *fake) { g.seen(t0.Add(40*day), "g-pay") }, 1},
		{"same features, a half-life newer", func(g *fake) { g.seen(t0.Add(31*day), "g-pay", "d-pay") }, 2},
		{"a shared member, a day newer", func(g *fake) {
			g.seen(t0.Add(3*day), "g-pay", "d-pay")
			g.facts = append(g.facts, member(t0.Add(3*day), "g-pay"), member(t0.Add(3*day), "d-pay"))
		}, 2},
		{"one side deleted", func(g *fake) {
			g.seen(t0.Add(40*day), "d-pay")
			g.seen(time.Time{}, "g-pay")
		}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, now := newFake(), t0.Add(2*day)
			cfg := Config{Now: func() time.Time { return now }}
			if _, err := Run(t.Context(), g, cfg); err != nil {
				t.Fatal(err)
			}
			tt.evolve(g)
			now = t0.Add(41 * day)
			if _, err := Run(t.Context(), g, cfg); err != nil {
				t.Fatal(err)
			}
			if len(g.links) != 1 || len(g.links[0].Deposits) != tt.want {
				t.Fatalf("got links %+v, want one link with %d deposits", g.links, tt.want)
			}
		})
	}
}

func TestRunStaleProposalDecaysBelowFloor(t *testing.T) {
	g, now := newFake(), t0.Add(2*day)
	cfg := Config{Now: func() time.Time { return now }}
	mustRun(t, g, cfg) // a deposit dated t0
	// strength 0.6/1.6 = 0.375; 31 days later 0.29/1.29 = 0.23: still open.
	now = t0.Add(31 * day)
	if res := mustRun(t, g, cfg); res.Closed != 0 || len(res.Review) != 0 {
		t.Fatalf("got closed %d, review %d after one half-life; want 0, 0", res.Closed, len(res.Review))
	}
	// 0.6 x 2^-3 = 0.075; strength 0.07 < 0.1.
	now = t0.Add(91 * day)
	if res := mustRun(t, g, cfg); res.Closed != 1 || g.links[0].ValidTo.IsZero() {
		t.Fatalf("got closed %d, link %+v; want the proposal closed", res.Closed, g.links[0])
	}
	// The same evidence does not bring it back.
	now = now.Add(day)
	if res := mustRun(t, g, cfg); res.Proposed != 0 || len(g.links) != 1 {
		t.Fatalf("closed proposal came back: %+v", g.links)
	}
	// Newer evidence on both sides proposes the pair again, as a new link.
	g.seen(now, "g-pay", "d-pay")
	now = now.Add(day)
	if res := mustRun(t, g, cfg); res.Proposed != 1 || res.Reinforced != 0 || len(g.links) != 2 {
		t.Fatalf("got proposed %d, reinforced %d, %d links; want a new proposal (1, 0, 2)", res.Proposed, res.Reinforced, len(g.links))
	}
}

func TestRunHalfLifeByKind(t *testing.T) {
	g, now := newFake(), t0.Add(2*day)
	cfg := Config{Now: func() time.Time { return now }, HalfLife: map[model.Kind]time.Duration{model.KindTeam: 2 * day}}
	if _, err := Run(t.Context(), g, cfg); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(day + 10*day) // five short half-lives
	res := mustRun(t, g, cfg)
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
			res := mustRun(t, g, Config{Now: func() time.Time { return t0.Add(2 * day) }})
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
	res, err := Run(t.Context(), g, cfg)
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
	res = mustRun(t, g, cfg)
	if res.Asserted != 0 {
		t.Fatalf("rerun asserted %d, want 0", res.Asserted)
	}
}

func TestRunRejectsBadRule(t *testing.T) {
	if _, err := Run(t.Context(), newFake(), Config{Rules: []KeyRule{{Name: "bad"}}}); err == nil {
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
	g.seen(t0, "gh-jane", "dir-jane", "t1", "t2")
	// Two different teams sharing their only member: name 0.x*0.6 + 0.3.
	g.facts = append(g.facts,
		graph.Fact{Subject: "gh-jane", Predicate: "member_of", Object: "t1", Sources: []graph.Source{{ObservedAt: t0}}},
		graph.Fact{Subject: "gh-jane", Predicate: "member_of", Object: "t2", Sources: []graph.Source{{ObservedAt: t0}}})

	res := mustRun(t, g, Config{Now: func() time.Time { return t0 }})
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
	res := mustRun(t, g, Config{Now: func() time.Time { return now }})
	if len(res.Review) != 2 || res.Review[0].Link.ID != "S" ||
		res.Review[0].Score.Strength <= res.Review[1].Score.Strength {
		t.Fatalf("got %+v", res.Review)
	}
}

// stubMatcher scores every candidate the same, under its own rule.
type stubMatcher struct{ score float64 }

func (stubMatcher) Rule() string { return "matcher/stub" }

func (m stubMatcher) Match(_ context.Context, cs []Candidate, _ Evidence) ([]Judgement, error) {
	out := make([]Judgement, 0, len(cs))
	for _, c := range cs {
		out = append(out, Judgement{Candidate: c, Score: m.score, Features: map[string]float64{"stub": m.score}})
	}
	return out, nil
}

func TestRunUsesTheConfiguredMatchers(t *testing.T) {
	for _, tt := range []struct {
		name  string
		score float64
		want  int
	}{
		{"above the bar proposes", 0.9, 1},
		{"below the bar proposes nothing", 0.1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := newFake()
			res, err := Run(t.Context(), g, Config{Now: func() time.Time { return t0.Add(2 * day) }, Matchers: []Matcher{stubMatcher{tt.score}}})
			if err != nil {
				t.Fatal(err)
			}
			if res.Proposed != tt.want {
				t.Fatalf("got %d proposals, want %d", res.Proposed, tt.want)
			}
			for _, l := range g.links {
				if l.Rule != "matcher/stub" {
					t.Errorf("got rule %q, want matcher/stub", l.Rule)
				}
			}
		})
	}
}

// mustRun runs Run and fails the test on an error.
func mustRun(t *testing.T, g IdentityGraph, cfg Config) Result {
	t.Helper()
	res, err := Run(t.Context(), g, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
