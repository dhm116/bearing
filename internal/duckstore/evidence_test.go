package duckstore

import (
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
	load(t, s, `INSERT INTO identity_link VALUES (?, ?, ?, ?, ?, 'r', TIMESTAMPTZ '2026-02-01 00:00:00+00', NULL)`, id, a, b, kind, status)
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
	ev := evidenceOf(t, s, graph.EvidenceRequest{From: []graph.EntityID{"svc-fe", "repo-spa"}, Predicates: []string{"owned_by"}})
	if len(ev.Relations) != 1 || ev.Relations[0].Kind != graph.RelationDistinct {
		t.Fatalf("got %+v, want one distinct relation", ev.Relations)
	}
	var ids []string
	for _, l := range ev.Relations[0].Links {
		ids = append(ids, l.ID)
	}
	if want := []string{"l-a", "l-p"}; !slices.Equal(ids, want) {
		t.Errorf("got links %v, want %v: the distinct_from, then the proposal", ids, want)
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
