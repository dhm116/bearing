package duckstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// shape writes a claim as "fact start>step>step Nf/Mh": steps are record IDs,
// a hop's links in brackets.
func shape(c graph.Claim) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", c.Fact.ID, c.Start)
	for _, st := range c.Path {
		b.WriteString(">" + string(st.To))
		if st.Fact == nil {
			fmt.Fprintf(&b, "[%s]", strings.Join(st.Links, ","))
		}
	}
	fmt.Fprintf(&b, " %df/%dh", c.Facts, c.SameAs)
	return b.String()
}

func shapes(ev graph.Evidence) []string {
	var out []string
	for _, c := range ev.Claims {
		out = append(out, shape(c))
	}
	return out
}

func evidenceOf(t *testing.T, s *Store, r graph.EvidenceRequest) graph.Evidence {
	t.Helper()
	ev, err := s.Evidence(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func wantShapes(t *testing.T, ev graph.Evidence, want ...string) {
	t.Helper()
	if got := shapes(ev); !slices.Equal(got, want) {
		t.Fatalf("got claims %q, want %q", got, want)
	}
}

// teams loads the ADR's frontend example: a repository (GitHub) and a service
// (PagerDuty), each owned by a team.
func teams(t *testing.T) *Store {
	t.Helper()
	s := newStore(t, Config{})
	load(t, s, `
INSERT INTO entity VALUES
 ('repo-spa', 'Repository', 'frontend-spa', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'repo-spa'),
 ('svc-fe', 'Component', 'frontend', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'svc-fe'),
 ('t-design', 'Team', 'design', TIMESTAMPTZ '2026-01-01 00:00:00+00', 't-design'),
 ('t-ui', 'Team', 'UI Team', TIMESTAMPTZ '2026-01-01 00:00:00+00', 't-ui');
INSERT INTO observation VALUES
 ('o-gh', 'github', 'Repository', 'repo:spa', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:01+00', NULL, '{}'),
 ('o-pd', 'pagerduty', 'Component', 'svc:fe', TIMESTAMPTZ '2026-01-01 00:00:00+00', TIMESTAMPTZ '2026-01-01 00:00:01+00', NULL, '{}');
INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source) VALUES
 ('f1', 'repo-spa', 'owned_by', 't-design', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'github'),
 ('f2', 'svc-fe', 'owned_by', 't-ui', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'pagerduty');
INSERT INTO fact_source VALUES ('f1', 'o-gh'), ('f2', 'o-pd');`)
	return s
}

func addLink(t *testing.T, s *Store, id, a, b, kind, status string) {
	t.Helper()
	load(t, s, `INSERT INTO identity_link (id, a, b, kind, status, rule, valid_from, valid_to) VALUES (?, ?, ?, ?, ?, 'r', TIMESTAMPTZ '2026-02-01 00:00:00+00', NULL)`, id, a, b, kind, status)
	load(t, s, `INSERT INTO link_deposit (link_id, deposited_at, weight, against) VALUES (?, TIMESTAMPTZ '2026-02-01 00:00:00+00', 0.9, false)`, id)
}

func TestEvidenceGivesEachSourcesClaimAndAProposedRelation(t *testing.T) {
	t.Parallel()
	s := teams(t)
	addLink(t, s, "l-p", "t-design", "t-ui", "same_as", "proposed")
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"svc-fe", "repo-spa"}, Predicates: []string{"owned_by"}, AnswerKind: model.KindTeam})
	wantShapes(t, ev, "f1 repo-spa>t-design 1f/0h", "f2 svc-fe>t-ui 1f/0h")
	if src := ev.Claims[0].Fact.Sources; len(src) != 1 || src[0].Source != "github" {
		t.Errorf("got sources %+v, want github's observation", src)
	}
	if len(ev.Relations) != 1 {
		t.Fatalf("got relations %+v, want one", ev.Relations)
	}
	rel := ev.Relations[0]
	if rel.A != "t-design" || rel.B != "t-ui" || rel.Kind != graph.RelationProposed || len(rel.Links) != 1 || rel.Links[0].ID != "l-p" || len(rel.Links[0].Deposits) != 1 {
		t.Errorf("got %+v, want a proposed relation t-design/t-ui with link l-p and its deposit", rel)
	}
	var recs []graph.EntityID
	for _, r := range ev.Records {
		recs = append(recs, r.ID)
	}
	if want := []graph.EntityID{"repo-spa", "svc-fe", "t-design", "t-ui"}; !slices.Equal(recs, want) {
		t.Errorf("got records %v, want %v", recs, want)
	}
	if ev.Records[0].Name != "frontend-spa" || ev.Records[0].Cluster != "repo-spa" {
		t.Errorf("got %+v, want the record's own name and cluster", ev.Records[0])
	}
}

func TestEvidenceHopsOnceToAJoinedRecordNamingItsLinks(t *testing.T) {
	t.Parallel()
	s := acme(t)
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-old"}, Predicates: []string{"owned_by", "language"}})
	wantShapes(t, ev, "f04 s-old>s-api[l1]>t-pay 1f/1h", "f08 s-old>s-api[l1]> 1f/1h")
	if ev.Claims[1].End != "" || ev.Claims[1].Path[1].To != "" {
		t.Errorf("got %+v, want an attribute claim with no end", ev.Claims[1])
	}
}

func TestEvidenceKeepsTheShortestPathWhenSeveralStartsReachAFact(t *testing.T) {
	t.Parallel()
	s := acme(t)
	for _, from := range [][]graph.EntityID{{"s-old", "s-api"}, {"s-api", "s-old"}} {
		ev := evidenceOf(t, s, graph.EvidenceRequest{From: from, Predicates: []string{"owned_by"}})
		wantShapes(t, ev, "f04 s-api>t-pay 1f/0h")
	}
}

func TestEvidenceFollowsFactsToTheDepthWithoutReturningToAClusterOnAPath(t *testing.T) {
	t.Parallel()
	s := acme(t)
	deps := []string{"depends_on"}
	// api -> ledger -> db -> ledger: the last would return to a cluster.
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-api"}, Predicates: deps, MaxDepth: 5})
	wantShapes(t, ev, "f01 s-api>s-ledger 1f/0h", "f02 s-api>s-ledger>s-db 2f/0h")
	if p := ev.Claims[1].Path; p[0].Fact.ID != "f01" || p[1].Fact.ID != "f02" {
		t.Errorf("got path facts %s, %s, want f01 then f02", p[0].Fact.ID, p[1].Fact.ID)
	}
	wantShapes(t, evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-api"}, Predicates: deps}), "f01 s-api>s-ledger 1f/0h")
}

func TestEvidenceReverseFindsDependents(t *testing.T) {
	t.Parallel()
	s := acme(t)
	deps := []string{"depends_on"}
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-ledger"}, Predicates: deps, Reverse: true})
	wantShapes(t, ev, "f01 s-ledger>s-api 1f/0h", "f03 s-ledger>s-db 1f/0h")
	if ev.Claims[0].End != "s-api" {
		t.Errorf("got end %s, want the dependent s-api", ev.Claims[0].End)
	}
	ev = evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-db"}, Predicates: deps, Reverse: true, MaxDepth: 3})
	wantShapes(t, ev, "f02 s-db>s-ledger 1f/0h", "f01 s-db>s-ledger>s-api 2f/0h")
}

func TestEvidenceAsOfATimeReadsFactsThatHeldThen(t *testing.T) {
	t.Parallel()
	s := acme(t)
	req := graph.EvidenceRequest{From: []graph.EntityID{"s-db"}, Predicates: []string{"owned_by"}}
	for _, tc := range []struct {
		name string
		at   time.Time
		want []string
	}{
		{"before it opened", time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC), nil},
		{"while it held", day(time.February, 1), []string{"f06 s-db>t-pay 1f/0h"}},
		{"after it closed", day(time.April, 1), nil},
		{"now", time.Time{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req.At = tc.at
			if got := shapes(evidenceOf(t, s, req)); !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEvidenceRelatesDistinctClustersWithTheirProposalsAfterIt(t *testing.T) {
	t.Parallel()
	s := teams(t)
	addLink(t, s, "l-p", "t-design", "t-ui", "same_as", "proposed")
	addLink(t, s, "l-a", "t-design", "t-ui", "distinct_from", "asserted")
	addLink(t, s, "l-s", "t-design", "t-ui", "same_as", "asserted") // refused: the pair is distinct
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"svc-fe", "repo-spa"}, Predicates: []string{"owned_by"}})
	if len(ev.Relations) != 1 || ev.Relations[0].Kind != graph.RelationDistinct {
		t.Fatalf("got %+v, want one distinct relation", ev.Relations)
	}
	var ids []string
	for _, l := range ev.Relations[0].Links {
		ids = append(ids, l.ID)
	}
	have := map[graph.EntityID]bool{}
	for _, r := range ev.Records {
		have[r.ID] = true
	}
	for _, l := range ev.Relations[0].Links {
		if !have[l.A] || !have[l.B] {
			t.Errorf("link %s names %s and %s, but Records has only %v", l.ID, l.A, l.B, have)
		}
	}
	if want := []string{"l-a", "l-s", "l-p"}; !slices.Equal(ids, want) {
		t.Errorf("got links %v, want %v: the distinct_from, the refused same_as, then the proposal", ids, want)
	}
}

func TestEvidenceRelatesAnAssertedSameAsTheStoreDidNotApply(t *testing.T) {
	t.Parallel()
	s := teams(t)
	// A Team and a Person never join one cluster, so the store left this link unapplied.
	load(t, s, `INSERT INTO entity VALUES ('p-ui', 'Person', 'UI', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'p-ui')`)
	load(t, s, `INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source)
		VALUES ('f3', 'repo-spa', 'owned_by', 'p-ui', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'github')`)
	addLink(t, s, "l-a", "p-ui", "t-design", "same_as", "asserted")
	addLink(t, s, "l-p", "p-ui", "t-design", "same_as", "proposed")
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"repo-spa"}, Predicates: []string{"owned_by"}})
	wantShapes(t, ev, "f1 repo-spa>t-design 1f/0h", "f3 repo-spa>p-ui 1f/0h")
	if len(ev.Relations) != 1 || ev.Relations[0].Kind != graph.RelationAssertedNotJoined || ev.Relations[0].A != "p-ui" || ev.Relations[0].Links[0].ID != "l-a" {
		t.Errorf("got %+v, want asserted_not_joined for p-ui/t-design led by l-a", ev.Relations)
	}
}

func TestEvidenceHasNoRelationForUnlinkedClosedOrJoinedEnds(t *testing.T) {
	t.Parallel()
	s := teams(t)
	both := graph.EvidenceRequest{From: []graph.EntityID{"svc-fe", "repo-spa"}}
	if ev := evidenceOf(t, s, both); len(ev.Relations) != 0 || len(ev.Claims) != 2 {
		t.Errorf("got %+v, want two claims and no relation", ev)
	}
	// A closed link is not open now.
	addLink(t, s, "l-p", "t-design", "t-ui", "same_as", "proposed")
	load(t, s, `UPDATE identity_link SET valid_to = TIMESTAMPTZ '2026-03-01 00:00:00+00'`)
	if ev := evidenceOf(t, s, both); len(ev.Relations) != 0 {
		t.Errorf("got %+v, want no relation from a closed link", ev.Relations)
	}
	// Ends in one cluster need none: the hops already show the join.
	load(t, s, `UPDATE entity SET cluster_id = 't-design' WHERE id = 't-ui'`)
	addLink(t, s, "l-j", "t-design", "t-ui", "same_as", "asserted")
	if ev := evidenceOf(t, s, both); len(ev.Relations) != 0 || len(ev.Claims) != 2 {
		t.Errorf("got %+v, want two claims and no relation", ev)
	}
}

func TestEvidenceAnswerKindTakesDescendantsRetiredKindsAndKindlessRangeMatches(t *testing.T) {
	t.Parallel()
	base, err := model.Core().Extend([]model.KindDef{
		{Name: "k8s.Service", Parent: model.KindComponent},
		{Name: "acme.Api", Parent: "k8s.Service"},
		{Name: "systemd.Unit", Parent: model.KindComponent},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := base.WithRetired([]model.KindDef{{Name: "old.Team", Parent: model.KindTeam, Retired: at(50)}}, nil)
	s := newStore(t, Config{Registry: reg})
	load(t, s, `
INSERT INTO entity VALUES
 ('c-src', 'Component', 'src', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'c-src'),
 ('k-1', 'k8s.Service', 'k', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'k-1'),
 ('a-1', 'acme.Api', 'a', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'a-1'),
 ('u-1', 'systemd.Unit', 'u', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'u-1'),
 ('t-old', 'old.Team', 'gone', TIMESTAMPTZ '2026-01-01 00:00:00+00', 't-old'),
 ('t-none', '', 'placeholder', TIMESTAMPTZ '2026-01-01 00:00:00+00', 't-none');
INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source) VALUES
 ('d1', 'c-src', 'depends_on', 'k-1', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 's'),
 ('d2', 'c-src', 'depends_on', 'a-1', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 's'),
 ('d3', 'c-src', 'depends_on', 'u-1', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 's'),
 ('m1', 'c-src', 'member_of', 't-old', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 's'),
 ('m2', 'c-src', 'member_of', 't-none', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 's'),
 ('d4', 'c-src', 'depends_on', 't-none', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 's');`)
	tests := []struct {
		name string
		pred string
		kind model.Kind
		want []string
	}{
		{"a kind and its descendants", "depends_on", "k8s.Service", []string{"d1", "d2"}},
		{"a leaf kind takes only itself", "depends_on", "acme.Api", []string{"d2"}},
		{"a retired kind under the answer kind, and a kindless record whose range is Team", "member_of", model.KindTeam, []string{"m1", "m2"}},
		{"a kindless record whose range is wider than the answer kind", "depends_on", model.KindComponent, []string{"d1", "d2", "d3"}},
		{"a kindless record out of range", "member_of", model.KindPerson, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"c-src"}, Predicates: []string{tc.pred}, AnswerKind: tc.kind})
			var got []string
			for _, c := range ev.Claims {
				got = append(got, c.Fact.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEvidenceReportsATableItCannotRead(t *testing.T) {
	t.Parallel()
	for _, drop := range []string{"entity", "fact", "identity_link", "link_deposit"} {
		t.Run(drop, func(t *testing.T) {
			t.Parallel()
			s := teams(t)
			addLink(t, s, "l-p", "t-design", "t-ui", "same_as", "proposed")
			execSQL(t, s, `DROP TABLE `+drop)
			_, err := s.Evidence(t.Context(), graph.EvidenceRequest{From: []graph.EntityID{"svc-fe", "repo-spa"}})
			if err == nil || !strings.HasPrefix(err.Error(), "evidence: ") {
				t.Fatalf("got %v, want an error from evidence", err)
			}
		})
	}
}

func TestEvidenceIgnoresProposedFactsAndUnknownStarts(t *testing.T) {
	t.Parallel()
	s := acme(t)
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-db", "nope"}, Predicates: []string{"owned_by"}})
	if len(ev.Claims) != 0 || len(ev.Relations) != 0 {
		t.Errorf("got %+v, want nothing: s-db's only open owner is a proposal", ev)
	}
}

func TestEvidenceHopsInAClusterReachedByAFact(t *testing.T) {
	t.Parallel()
	s := acme(t)
	// s-ledger2 joins s-ledger's cluster; the fact into s-ledger came first, and s-ledger2 holds the next.
	load(t, s, `
INSERT INTO entity VALUES
 ('s-ledger2', 'Service', 'ledger-2', TIMESTAMPTZ '2026-01-02 00:00:00+00', 's-ledger'),
 ('s-cache', 'Service', 'cache', TIMESTAMPTZ '2026-01-01 00:00:00+00', 's-cache');
INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source)
 VALUES ('f20', 's-ledger2', 'depends_on', 's-cache', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'gh')`)
	addLink(t, s, "l-ly", "s-ledger", "s-ledger2", "same_as", "asserted")
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-api"}, Predicates: []string{"depends_on"}, MaxDepth: 2})
	wantShapes(t, ev, "f01 s-api>s-ledger 1f/0h", "f02 s-api>s-ledger>s-db 2f/0h", "f20 s-api>s-ledger>s-ledger2[l-ly]>s-cache 2f/1h")
}

func TestEvidenceHopNamesEveryLinkOfAChain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		direct bool
		want   string
	}{
		{"a chain of two", false, "f9 r-a>r-c[l-ab,l-bc]>t-design 1f/1h"},
		{"a direct link beats the chain", true, "f9 r-a>r-c[l-ac]>t-design 1f/1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := teams(t)
			load(t, s, `
INSERT INTO entity VALUES
 ('r-a', 'Repository', 'a', TIMESTAMPTZ '2026-01-01 00:00:00+00', 'r-a'),
 ('r-b', 'Repository', 'b', TIMESTAMPTZ '2026-01-02 00:00:00+00', 'r-a'),
 ('r-c', 'Repository', 'c', TIMESTAMPTZ '2026-01-03 00:00:00+00', 'r-a');
INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source)
 VALUES ('f9', 'r-c', 'owned_by', 't-design', NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 'github')`)
			addLink(t, s, "l-ab", "r-a", "r-b", "same_as", "asserted")
			addLink(t, s, "l-bc", "r-b", "r-c", "same_as", "asserted")
			if tc.direct {
				addLink(t, s, "l-ac", "r-a", "r-c", "same_as", "asserted")
			}
			ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"r-a"}, Predicates: []string{"owned_by"}})
			wantShapes(t, ev, tc.want)
		})
	}
}

func TestEvidenceClampsMaxDepthAndTreatsNegativeAsOne(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	for i := range 13 {
		load(t, s, `INSERT INTO entity VALUES (?, 'Component', 'c', TIMESTAMPTZ '2026-01-01 00:00:00+00', ?)`, fmt.Sprintf("c%02d", i), fmt.Sprintf("c%02d", i))
		if i > 0 {
			load(t, s, `INSERT INTO fact (id, subject_id, predicate, object_id, value, status, confidence, valid_from, valid_to, source)
				VALUES (?, ?, 'depends_on', ?, NULL, 'asserted', 1, TIMESTAMPTZ '2026-01-01 00:00:00+00', NULL, 's')`,
				fmt.Sprintf("d%02d", i), fmt.Sprintf("c%02d", i-1), fmt.Sprintf("c%02d", i))
		}
	}
	for depth, want := range map[int]int{99: 10, -3: 1, 0: 1, 3: 3} {
		ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"c00"}, MaxDepth: depth})
		if len(ev.Claims) != want {
			t.Errorf("MaxDepth %d got %d claims, want %d", depth, len(ev.Claims), want)
		}
	}
}

func TestEvidenceReverseOverAnAttributeGivesNoClaim(t *testing.T) {
	t.Parallel()
	s := acme(t)
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-api"}, Predicates: []string{"language"}, Reverse: true})
	if len(ev.Claims) != 0 {
		t.Errorf("got %q, want no claim", shapes(ev))
	}
}

func TestEvidenceKeepsAFactsQualifiersAndValidTo(t *testing.T) {
	t.Parallel()
	s := acme(t)
	load(t, s, `UPDATE fact SET qualifiers = '{"port": 8080}' WHERE id = 'f06'`)
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"s-db"}, Predicates: []string{"owned_by"}, At: day(time.February, 1)})
	if len(ev.Claims) != 1 {
		t.Fatalf("got %q, want f06", shapes(ev))
	}
	f := ev.Claims[0].Fact
	if f.Qualifiers["port"] != float64(8080) || !f.ValidTo.Equal(day(time.March, 1)) || !f.ValidFrom.Equal(day(time.January, 1)) {
		t.Errorf("got qualifiers %v, valid %v to %v, want port 8080 and Jan 1 to Mar 1", f.Qualifiers, f.ValidFrom, f.ValidTo)
	}
}

func TestEvidenceFailsOnACancelledContext(t *testing.T) {
	t.Parallel()
	s := teams(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Evidence(ctx, graph.EvidenceRequest{From: []graph.EntityID{"svc-fe"}}); err == nil {
		t.Fatal("got no error, want one from the cancelled context")
	}
}

func TestEvidenceShowsEveryObservationSourceOfAFact(t *testing.T) {
	t.Parallel()
	s := teams(t)
	load(t, s, `INSERT INTO observation VALUES ('o-bs', 'backstage', 'Repository', 'repo:spa', TIMESTAMPTZ '2026-01-02 00:00:00+00', TIMESTAMPTZ '2026-01-02 00:00:01+00', NULL, '{}')`)
	load(t, s, `INSERT INTO fact_source VALUES ('f1', 'o-bs')`)
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"repo-spa"}, Predicates: []string{"owned_by"}})
	var got []string
	for _, src := range ev.Claims[0].Fact.Sources {
		got = append(got, src.Source)
	}
	if want := []string{"github", "backstage"}; !slices.Equal(got, want) || ev.Claims[0].Fact.SourceCount != 2 {
		t.Errorf("got sources %v (count %d), want %v", got, ev.Claims[0].Fact.SourceCount, want)
	}
}
