package query_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
)

// recordWorld is a store the ADR 11 tests write to directly.
type recordWorld struct {
	t *testing.T
	s *duckstore.Store
	q *query.Querier
}

func newRecordWorld(t *testing.T) *recordWorld {
	t.Helper()
	s, err := duckstore.Open(context.Background(), duckstore.Config{Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &recordWorld{t: t, s: s, q: &query.Querier{Graph: s, Now: func() time.Time { return at(100) }}}
}

func (w *recordWorld) ingest(source string, obs ...*eventv1alpha1.Observation) {
	w.t.Helper()
	if _, err := w.s.Ingest(context.Background(), graph.IngestRequest{Source: source, Observations: obs}); err != nil {
		w.t.Fatal(err)
	}
}

// rec is the ID of the record a key names.
func (w *recordWorld) rec(key string) graph.EntityID {
	w.t.Helper()
	e, err := w.s.Entity(context.Background(), key)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, r := range e.Records {
		if slices.Contains(r.Keys, model.Key(key)) {
			return r.ID
		}
	}
	w.t.Fatalf("no record has key %s", key)
	return ""
}

func (w *recordWorld) link(a, b string, kind graph.LinkKind, status graph.Status, n int, features map[string]float64) {
	w.t.Helper()
	p := graph.Proposal{
		A: w.rec(a), B: w.rec(b), Kind: kind, Status: status, Rule: "rule/test",
		Deposit: graph.Deposit{At: at(n), Weight: 0.9, Features: features},
	}
	if _, err := w.s.Link(context.Background(), []graph.Proposal{p}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *recordWorld) decide(d graph.Decision) {
	w.t.Helper()
	if _, err := w.s.Decide(context.Background(), d); err != nil {
		w.t.Fatal(err)
	}
}

// claimText is a claim as "start>object by source [hops]".
func claimText(c query.Claim) string {
	var srcs []string
	for _, s := range c.Fact.Sources {
		srcs = append(srcs, s.Source)
	}
	obj := ""
	if c.Fact.Object != nil {
		obj = c.Fact.Object.Key
	}
	return fmt.Sprintf("%s>%s>%s by %v %dh", c.Start.Key, c.Fact.Subject.Key, obj, srcs, c.SameAs)
}

func claimTexts(cs []query.Claim) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = claimText(c)
	}
	return out
}

// TestOwnersGivesEachSourcesClaimWithItsRecordAndCluster is ADR 11's
// frontend example: GitHub's repository and the directory's service are one
// cluster, and each names its own team; the teams are likely the same.
func TestOwnersGivesEachSourcesClaimWithItsRecordAndCluster(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	w.ingest("github", ob("g1", 1, "Team", "github:team/design", attr("name", "design")),
		ob("g2", 1, "Repository", "github:repo/frontend-spa", attr("name", "frontend-spa"), rel("owned_by", "github:team/design", false)))
	w.ingest("pager", ob("p1", 2, "Team", "pager:team/ui", attr("name", "UI Team")),
		ob("p2", 2, "Repository", "pager:service/frontend", attr("name", "frontend"), aliased("github:repo/frontend-spa"), rel("owned_by", "pager:team/ui", false)))
	w.link("github:team/design", "pager:team/ui", graph.SameAs, graph.Proposed, 3, map[string]float64{"shared_members": 0.92})

	o, err := w.q.Owners(t.Context(), "github:repo/frontend-spa", time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"github:repo/frontend-spa>github:repo/frontend-spa>github:team/design by [github] 0h",
		"github:repo/frontend-spa>pager:service/frontend>pager:team/ui by [pager] 1h",
	}
	if got := claimTexts(o.Claims); !slices.Equal(got, want) {
		t.Fatalf("got claims %q, want %q", got, want)
	}
	gh, pd := o.Claims[0], o.Claims[1]
	if gh.Fact.Subject.Cluster == "" || gh.Fact.Subject.Cluster != pd.Fact.Subject.Cluster || gh.Fact.Subject.ID == pd.Fact.Subject.ID {
		t.Errorf("got subjects %+v and %+v, want two records of one cluster", gh.Fact.Subject, pd.Fact.Subject)
	}
	if pd.Fact.Subject.Name != "frontend" || pd.Fact.Object.Name != "UI Team" {
		t.Errorf("got %s owned by %s, want each record named by its own name", pd.Fact.Subject.Name, pd.Fact.Object.Name)
	}
	hop := pd.Path[0]
	if hop.FactID != "" || len(hop.Links) != 1 || hop.To.ID != pd.Fact.Subject.ID || pd.Path[1].FactID != pd.Fact.ID {
		t.Errorf("got path %+v, want a hop naming its link, then the fact", pd.Path)
	}
	if len(o.Relations) != 1 {
		t.Fatalf("got relations %+v, want one", o.Relations)
	}
	r := o.Relations[0]
	clusters := []string{gh.Fact.Object.Cluster, pd.Fact.Object.Cluster}
	slices.Sort(clusters)
	if r.Kind != query.RelationLikelySame || !slices.Equal(r.Clusters[:], clusters) || len(r.Links) != 1 || r.Links[0].Score.Strength <= 0 {
		t.Errorf("got relation %+v, want the two teams' clusters likely the same, scored", r)
	}
}

// TestRelationsFollowTheFirstRuleThatApplies covers ADR 11's relation rules 1
// and 2: an asserted distinct_from wins, with proposals listed after it, and
// an asserted same_as the store refused is asserted but not joined.
func TestRelationsFollowTheFirstRuleThatApplies(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		link       func(w *recordWorld)
		kind       string
		reason     string
		linkStatus []string
	}{
		{"distinct wins over a proposal", func(w *recordWorld) {
			w.link("a:team/1", "b:team/2", graph.DistinctFrom, graph.Asserted, 3, nil)
			w.link("a:team/1", "b:team/2", graph.SameAs, graph.Proposed, 4, nil)
		}, query.RelationDistinct, "", []string{"asserted", "proposed"}},
		{"proposals strongest first", func(w *recordWorld) {
			w.link("a:team/1", "b:team/2", graph.DistinctFrom, graph.Proposed, 3, nil)
			w.link("a:team/1", "b:team/2", graph.SameAs, graph.Proposed, 50, nil) // newer evidence, less decayed
		}, query.RelationLikelySame, "", []string{"proposed", "proposed"}},
		{"a refused same_as across a kind line", func(w *recordWorld) {
			w.link("a:team/1", "b:person/2", graph.SameAs, graph.Asserted, 3, nil)
		}, query.RelationAssertedNotJoined, query.ReasonKinds, []string{"asserted"}},
		{"a proposed distinct_from", func(w *recordWorld) {
			w.link("a:team/1", "b:team/2", graph.DistinctFrom, graph.Proposed, 3, nil)
		}, query.RelationLikelyDistinct, "", []string{"proposed"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newRecordWorld(t)
			w.ingest("a", ob("a1", 1, "Team", "a:team/1", attr("name", "one")),
				ob("a2", 1, "Repository", "a:repo/x", rel("owned_by", "a:team/1", false), rel("owned_by", "b:team/2", false), rel("owned_by", "b:person/2", false)))
			w.ingest("b", ob("b1", 1, "Team", "b:team/2", attr("name", "two")), ob("b2", 1, "Person", "b:person/2", attr("name", "two")))
			tt.link(w)
			o, err := w.q.Owners(t.Context(), "a:repo/x", time.Time{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(o.Relations) != 1 {
				t.Fatalf("got relations %+v, want one", o.Relations)
			}
			r := o.Relations[0]
			var statuses []string
			for _, l := range r.Links {
				statuses = append(statuses, l.Status)
			}
			if r.Kind != tt.kind || r.Reason != tt.reason || !slices.Equal(statuses, tt.linkStatus) {
				t.Errorf("got %s (%s) with links %v, want %s (%s) with %v", r.Kind, r.Reason, statuses, tt.kind, tt.reason, tt.linkStatus)
			}
		})
	}
}

// TestSameAsHopListsOnlyTheLinksTheStoreApplied asserts b same_as c, which a
// distinct_from between a and c refuses: a's hop to b names only the link
// that joined them, and c stays a cluster of its own.
func TestSameAsHopListsOnlyTheLinksTheStoreApplied(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	w.ingest("s", ob("s1", 1, "Repository", "s:repo/a"), ob("s2", 1, "Repository", "s:repo/b", rel("owned_by", "s:team/t", false)),
		ob("s3", 1, "Repository", "s:repo/c", rel("owned_by", "s:team/u", false)), ob("s4", 1, "Team", "s:team/t"), ob("s5", 1, "Team", "s:team/u"))
	w.link("s:repo/a", "s:repo/c", graph.DistinctFrom, graph.Asserted, 2, nil)
	w.link("s:repo/a", "s:repo/b", graph.SameAs, graph.Asserted, 3, nil)
	w.link("s:repo/b", "s:repo/c", graph.SameAs, graph.Asserted, 4, nil)
	ls, err := w.s.Links(t.Context(), graph.LinkFilter{Kind: graph.SameAs})
	if err != nil || len(ls) != 2 {
		t.Fatalf("got %+v, %v, want two same_as links", ls, err)
	}
	o, err := w.q.Owners(t.Context(), "s:repo/a", time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := claimTexts(o.Claims); !slices.Equal(got, []string{"s:repo/a>s:repo/b>s:team/t by [s] 1h"}) {
		t.Fatalf("got claims %q, want only b's owner, one hop away", got)
	}
	applied := ls[0].ID // a-b opened first
	if hop := o.Claims[0].Path[0]; !slices.Equal(hop.Links, []string{applied}) {
		t.Errorf("got hop links %v, want only %s, which the store applied", hop.Links, applied)
	}
}

// TestSubjectsSayHowTheyMatched asks by key, by exact name and by a near
// name, and past the lookup's limit.
func TestSubjectsSayHowTheyMatched(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	obs := []*eventv1alpha1.Observation{ob("t", 1, "Team", "g:team/t", attr("name", "Team"))}
	for i := range query.MaxSubjects + 1 {
		obs = append(obs, ob(fmt.Sprintf("r%d", i), 1, "Repository", fmt.Sprintf("g:repo/r%d", i), attr("name", fmt.Sprintf("crowd-%02d", i))))
	}
	obs = append(obs, ob("p", 1, "Repository", "g:repo/payments", attr("name", "payments"), rel("owned_by", "g:team/t", false)))
	w.ingest("g", obs...)
	for _, tt := range []struct {
		ref, match string
		score      func(float64) bool
		limitHit   bool
	}{
		{"g:repo/payments", query.MatchKey, func(s float64) bool { return s == 1 }, false},
		{"Payments", query.MatchName, func(s float64) bool { return s == 1 }, false},
		{"paymnts", query.MatchFuzzy, func(s float64) bool { return s > 0.8 && s < 1 }, false},
		{"crowd", query.MatchFuzzy, func(s float64) bool { return s < 1 }, true},
	} {
		t.Run(tt.ref, func(t *testing.T) {
			o, err := w.q.Owners(t.Context(), tt.ref, time.Time{}, false)
			if err != nil {
				t.Fatal(err)
			}
			s := o.Subjects[0]
			if s.Match != tt.match || !tt.score(s.Score) || o.LimitHit != tt.limitHit {
				t.Errorf("got %s %v, limit hit %v, want %s, limit hit %v", s.Match, s.Score, o.LimitHit, tt.match, tt.limitHit)
			}
			if tt.limitHit && len(o.Subjects) != query.MaxSubjects {
				t.Errorf("got %d subjects, want the limit, %d", len(o.Subjects), query.MaxSubjects)
			}
			if !tt.limitHit && (len(o.Claims) != 1 || o.Claims[0].Start.Match != tt.match) {
				t.Errorf("got claims %+v, want one claim carrying the match", o.Claims)
			}
		})
	}
}

// changeTexts are changes as "kind time object".
func changeTexts(cs []query.Change) []string {
	var out []string
	for _, ch := range cs {
		out = append(out, fmt.Sprintf("%s %s %s", ch.Kind, ch.At.Format(time.TimeOnly), ch.Fact.Object.Key))
	}
	return out
}

// TestAPastAnswerShowsWhatChangedSince asks as of a past time: a claim that
// closed since carries its closing, and the facts today's answer has that
// opened since are the answer's, once each, even when the past answer was
// empty or today's answer falls back to another predicate.
func TestAPastAnswerShowsWhatChangedSince(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		later      []*eventv1alpha1.Observation
		wantClaims []string
		wantClosed []string
		wantOpened []string
	}{
		{
			"CODEOWNERS moves to platform",
			[]*eventv1alpha1.Observation{ob("r2", 10, "Repository", payments, rel("approves_changes", teamPay, true), rel("approves_changes", teamPlat, false))},
			[]string{payments + ">" + payments + ">" + teamPay + " by [g] 0h"},
			[]string{"closed 00:10:00 " + teamPay},
			[]string{"opened 00:10:00 " + teamPlat},
		},
		{
			"an owned_by appears where only CODEOWNERS answered",
			[]*eventv1alpha1.Observation{ob("r2", 10, "Repository", payments, rel("owned_by", teamPlat, false))},
			[]string{payments + ">" + payments + ">" + teamPay + " by [g] 0h"},
			nil,
			[]string{"opened 00:10:00 " + teamPlat},
		},
		{
			// Within one observation platform's fact gets the smaller ID, and
			// zeta sorts last by key: only the order rules put them as wanted.
			"several owners open since, newest first, then by ADR 11's order",
			[]*eventv1alpha1.Observation{
				ob("r2", 10, "Repository", payments, rel("owned_by", teamPlat, false), rel("owned_by", teamPay, false)),
				ob("r3", 20, "Repository", payments, rel("owned_by", "github:team/acme/zeta", false)),
			},
			[]string{payments + ">" + payments + ">" + teamPay + " by [g] 0h"},
			nil,
			[]string{"opened 00:20:00 github:team/acme/zeta", "opened 00:10:00 " + teamPay, "opened 00:10:00 " + teamPlat},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newRecordWorld(t)
			w.ingest("g", ob("t1", 1, "Team", teamPay, attr("name", "Payments")), ob("t2", 1, "Team", teamPlat, attr("name", "Platform")),
				ob("r1", 1, "Repository", payments, rel("approves_changes", teamPay, false)))
			w.ingest("g", tt.later...)
			o, err := w.q.Owners(t.Context(), payments, at(5), false)
			if err != nil {
				t.Fatal(err)
			}
			if got := claimTexts(o.Claims); !slices.Equal(got, tt.wantClaims) {
				t.Fatalf("got claims %q, want %q", got, tt.wantClaims)
			}
			if got := changeTexts(o.Claims[0].ChangedSince); !slices.Equal(got, tt.wantClosed) {
				t.Errorf("got the claim's changes %q, want %q", got, tt.wantClosed)
			}
			if got := changeTexts(o.ChangedSince); !slices.Equal(got, tt.wantOpened) {
				t.Errorf("got the answer's changes %q, want %q", got, tt.wantOpened)
			}
			now, err := w.q.Owners(t.Context(), payments, time.Time{}, false)
			if err != nil || now.ChangedSince != nil || now.Claims[0].ChangedSince != nil {
				t.Errorf("got %+v, %v, want today's answer to carry no changes", now, err)
			}
		})
	}
	t.Run("nothing owned it then", func(t *testing.T) {
		t.Parallel()
		w := newRecordWorld(t)
		w.ingest("g", ob("t1", 1, "Team", teamPay), ob("r1", 1, "Repository", payments))
		w.ingest("g", ob("r2", 10, "Repository", payments, rel("owned_by", teamPay, false)), ob("r3", 10, "Repository", billing, rel("owned_by", teamPay, false)))
		o, err := w.q.Owners(t.Context(), payments, at(5), false)
		if err != nil {
			t.Fatal(err)
		}
		if len(o.Claims) != 0 || !slices.Equal(changeTexts(o.ChangedSince), []string{"opened 00:10:00 " + teamPay}) {
			t.Errorf("got claims %q and changes %q, want none then and payments' owner opened since, once", claimTexts(o.Claims), changeTexts(o.ChangedSince))
		}
	})
}

// TestAKindlessOwnerTakesItsClustersKind is CODEOWNERS naming @jdoe and
// @payments by node keys no source has described: each owner is a
// placeholder with no kind, joined to a Person and a Team. The person is never
// listed as a team, and the team still reads as one.
func TestAKindlessOwnerTakesItsClustersKind(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	// The person and team exist under their own keys before CODEOWNERS names
	// their node keys, so when a later sync aliases the node keys, a link
	// joins each placeholder and none is taken over.
	const jdoe, team = "github:user_node/U1", "github:team_node/T1"
	w.ingest("g", ob("p1", 1, "Person", "github:user/jdoe", attr("name", "Jane Doe")), ob("t1", 1, "Team", teamPay, attr("name", "Payments")))
	w.ingest("g", ob("r1", 2, "Repository", payments, rel("approves_changes", jdoe, false), rel("approves_changes", team, false)))
	w.ingest("g", ob("p2", 3, "Person", "github:user/jdoe", aliased(jdoe)), ob("t2", 3, "Team", teamPay, aliased(team)))
	o, err := w.q.Owners(t.Context(), payments, time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, c := range o.Claims {
		kinds[c.Fact.Object.Key] = c.Fact.Object.Kind + "/" + c.Fact.Object.ClusterKind
	}
	if kinds[jdoe] != "/Person" || kinds[team] != "/Team" {
		t.Fatalf("got object kinds %v, want the placeholders kindless with their clusters' kinds", kinds)
	}
	if len(o.Teams) != 1 || o.Teams[0].Team.Key != team {
		t.Errorf("got teams %+v, want only the payments team, never jdoe", o.Teams)
	}
	byPlaceholder, err := w.q.Owners(t.Context(), jdoe, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := byPlaceholder.Subjects[0]; s.Key != jdoe || s.Kind != "" || s.ClusterKind != "Person" {
		t.Errorf("asked by the placeholder's key: got subject %+v, want it kindless in a Person cluster", s.Ref)
	}
}

// TestRelatedWithoutAPredicateGivesOnlyRelations: an attribute fact is no
// claim of related.
func TestRelatedWithoutAPredicateGivesOnlyRelations(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	w.ingest("g", ob("r1", 1, "Repository", payments, attr("language", "Go"), rel("depends_on", ledger, false)))
	r, err := w.q.Related(t.Context(), payments, "", "out", 1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := claimTexts(r.Claims); !slices.Equal(got, []string{payments + ">" + payments + ">" + ledger + " by [g] 0h"}) {
		t.Errorf("got claims %q, want only depends_on ledger", got)
	}
}

// identityText is an identity change as "kind a-b rule actor reason rows".
func identityText(c query.IdentityChange) string {
	var rows []string
	for _, l := range c.Links {
		rows = append(rows, l.Kind+" "+l.Change+" "+l.Effect)
	}
	j := c.Justification
	return fmt.Sprintf("%s %s-%s %s %s %q %v", c.Kind, c.A.Key, c.B.Key, j.Rule, j.Actor, j.Reason, rows)
}

// TestChangesListJoinsAndSplitsOncePerDecision covers Q3's identity events:
// a person's join over a distinct_from is one event of two rows, a split is
// one, an asserted same_as a distinct_from refuses is asserted but not
// joined, and proposals never appear.
func TestChangesListJoinsAndSplitsOncePerDecision(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	w.ingest("a", ob("a1", 1, "Team", "a:team/1"), ob("a2", 1, "Team", "a:team/3"))
	w.ingest("b", ob("b1", 1, "Team", "b:team/2"), ob("b2", 1, "Team", "b:team/4"))
	w.link("a:team/1", "b:team/2", graph.DistinctFrom, graph.Asserted, 2, nil)
	w.decide(graph.Decision{ID: "d-join", Kind: graph.Join, A: "a:team/1", B: "b:team/2", Actor: "alice", Reason: "one team", At: at(5)})
	w.decide(graph.Decision{ID: "d-split", Kind: graph.Split, A: "a:team/1", B: "b:team/2", Actor: "bob", Reason: "two teams", At: at(7)})
	w.link("a:team/3", "b:team/4", graph.DistinctFrom, graph.Asserted, 8, nil)
	w.link("a:team/3", "b:team/4", graph.SameAs, graph.Asserted, 9, nil)
	w.link("a:team/1", "b:team/4", graph.SameAs, graph.Proposed, 9, nil)

	c, err := w.q.Changes(t.Context(), "", at(4), at(20), 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range c.Identity {
		got = append(got, identityText(ev))
	}
	want := []string{
		`asserted_not_joined a:team/3-b:team/4 rule/test  "" [same_as opened refused]`,
		`split a:team/1-b:team/2 person bob "two teams" [same_as closed split distinct_from opened split]`,
		`joined a:team/1-b:team/2 person alice "one team" [distinct_from closed joined same_as opened joined]`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got identity changes\n%q\nwant\n%q", got, want)
	}
	if j := c.Identity[2].Justification; j.Decision != "d-join" {
		t.Errorf("got decision %q, want d-join", j.Decision)
	}
}

// TestAnswersReportAStoreThatFails closes the store under each question:
// the error comes back, and nothing is answered from half a read.
func TestAnswersReportAStoreThatFails(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	w.ingest("g", ob("r1", 1, "Repository", payments, attr("name", "payments"), rel("owned_by", teamPay, false)))
	if err := w.s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for name, ask := range map[string]func() (any, error){
		"owners by key":  func() (any, error) { return w.q.Owners(ctx, payments, time.Time{}, true) },
		"owners by name": func() (any, error) { return w.q.Owners(ctx, "payments", time.Time{}, false) },
		"related":        func() (any, error) { return w.q.Related(ctx, payments, "", "in", 2, at(5)) },
		"changes":        func() (any, error) { return w.q.Changes(ctx, "", at(0), at(10), 0) },
		"runtimes":       func() (any, error) { return w.q.Runtimes(ctx, query.RuntimesRequest{Language: "go"}) },
		"links":          func() (any, error) { return w.q.Links(ctx, query.LinksRequest{}) },
	} {
		if _, err := ask(); err == nil || errors.Is(err, query.ErrNotFound) {
			t.Errorf("%s on a closed store: got %v, want the store's error", name, err)
		}
	}
}

// TestSubjectsOfANameNothingResemblesAreNotFound: a name lookup that finds
// nothing is not found, like an unknown key.
func TestSubjectsOfANameNothingResemblesAreNotFound(t *testing.T) {
	t.Parallel()
	w := newRecordWorld(t)
	w.ingest("g", ob("r1", 1, "Repository", payments, attr("name", "payments")))
	if _, err := w.q.Owners(t.Context(), "zzqx", time.Time{}, false); !errors.Is(err, query.ErrNotFound) {
		t.Errorf("got %v, want not found", err)
	}
}
