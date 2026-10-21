package duckstore

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

func linked(k ...string) func(*modelv1alpha1.ObservationData) {
	return func(d *modelv1alpha1.ObservationData) { d.Entity.LinkedIds = k }
}

// vouchesForNodes is an Authoritative that vouches only for *_node keys.
func vouchesForNodes(k model.Key) bool {
	_, kt, _, err := k.Parse()
	return err == nil && strings.HasSuffix(kt, "_node")
}

func ingestReq(t *testing.T, s *Store, r graph.IngestRequest) graph.IngestResult {
	t.Helper()
	res, err := s.Ingest(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// clusterOf is the entity ID a ref resolves to.
func clusterOf(t *testing.T, s *Store, ref string) graph.EntityID {
	t.Helper()
	e, err := s.Entity(t.Context(), ref)
	if err != nil {
		t.Fatalf("entity %s: %v", ref, err)
	}
	return e.ID
}

// wantClusters checks which cluster each ref resolves to.
func wantClusters(t *testing.T, s *Store, want map[string]graph.EntityID) {
	t.Helper()
	for _, ref := range slices.Sorted(maps.Keys(want)) {
		if got := clusterOf(t, s, ref); got != want[ref] {
			t.Errorf("%s: got cluster %s, want %s", ref, got, want[ref])
		}
	}
}

func links(t *testing.T, s *Store, f graph.LinkFilter) []graph.Link {
	t.Helper()
	ls, err := s.Links(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

func link(t *testing.T, s *Store, ps ...graph.Proposal) graph.LinkResult {
	t.Helper()
	r, err := s.Link(t.Context(), ps)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func sameAs(a, b string, st graph.Status, rule string) graph.Proposal {
	return graph.Proposal{
		A: graph.EntityID(a), B: graph.EntityID(b), Kind: graph.SameAs, Status: st, Rule: rule,
		Deposit: graph.Deposit{At: at(50), Weight: 1},
	}
}

// people ingests one Person record per key, IDs id-1, id-2, ... in order.
func people(t *testing.T, s *Store, keys ...string) {
	t.Helper()
	var obs []*eventv1alpha1.Observation
	for i, k := range keys {
		obs = append(obs, obs1(k, i, "Person", k))
	}
	ingestOK(t, s, "src", obs...)
}

func obs1(id string, n int, kind, key string, opts ...func(*modelv1alpha1.ObservationData)) *eventv1alpha1.Observation {
	return obs("o-"+id, n, kind, key, append(opts, attrs("name", tail(key)))...)
}

func TestIngestSharedKeyLinksRecordsAndReadsAnswerForTheCluster(t *testing.T) {
	ctx := t.Context()
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "gh:repo/api", rel("depends_on", "k:a/db", false))) // id-1, id-2, fact id-3
	ingestOK(t, s, "bs", obs("o2", 1, "Component", "bs:comp/api", attrs("language", "Go")))            // id-4, fact id-5
	r := ingestOK(t, s, "bs", obs("o3", 3, "Component", "bs:comp/api", aliases("gh:repo/api")))
	if r.Entities != 0 {
		t.Fatalf("got %+v, want no new record and no merge", r)
	}
	ls := links(t, s, graph.LinkFilter{Entity: "bs:comp/api"})
	if len(ls) != 1 || ls[0].Rule != "shared_key" || ls[0].Status != graph.Asserted || ls[0].Kind != graph.SameAs ||
		len(ls[0].Deposits) != 1 || ls[0].Deposits[0].ObservationID != "o3" || !ls[0].ValidFrom.Equal(at(3)) {
		t.Fatalf("got links %+v, want one asserted shared_key same_as with o3 as evidence", ls)
	}
	wantClusters(t, s, map[string]graph.EntityID{"gh:repo/api": "id-1", "bs:comp/api": "id-1", "id-4": "id-1"})

	e, err := s.Entity(ctx, "id-4")
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "Component" || e.Name != "api" || !slices.Equal(e.Keys, []model.Key{"bs:comp/api", "gh:repo/api"}) || len(e.Records) != 2 {
		t.Fatalf("got %+v, want the cluster of both records with both keys", e)
	}
	if !slices.Equal(e.Records[0].Sources, []string{"gh"}) || !slices.Equal(e.Records[1].Sources, []string{"bs"}) {
		t.Errorf("got record sources %v and %v, want gh and bs", e.Records[0].Sources, e.Records[1].Sources)
	}
	// The key stays on its record and the facts stay where they were observed.
	if n := count(t, s, `SELECT count(*) FROM alias WHERE key = 'gh:repo/api' AND entity_id = 'id-1'`); n != 1 {
		t.Errorf("got %d, want gh:repo/api still on id-1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM fact WHERE subject_id = 'id-4'`); n != 1 {
		t.Errorf("got %d facts on id-4, want its 1", n)
	}

	facts, err := s.Facts(ctx, graph.FactFilter{Subject: "id-4"})
	if err != nil {
		t.Fatal(err)
	}
	if ids := factIDs(facts); !slices.Equal(ids, []string{"id-3", "id-5"}) || facts[0].Subject != "id-1" || facts[1].Subject != "id-1" {
		t.Fatalf("got %+v, want both records' facts under id-1", facts)
	}
	steps, err := s.Walk(ctx, graph.WalkRequest{From: "id-4", Predicates: []string{"depends_on"}})
	if err != nil || len(steps) != 1 || steps[0].Fact.Subject != "id-1" || steps[0].Fact.Object != "id-2" {
		t.Fatalf("Walk: got %+v, %v, want depends_on from id-1 to id-2", steps, err)
	}
	in, err := s.Walk(ctx, graph.WalkRequest{From: "id-2", Direction: graph.In})
	if err != nil || len(in) != 1 || in[0].Fact.Subject != "id-1" {
		t.Fatalf("Walk in: got %+v, %v, want one step back to id-1", in, err)
	}
	cs, err := s.Changes(ctx, graph.ChangesRequest{Subject: "id-4"})
	if err != nil || len(cs) != 2 {
		t.Fatalf("Changes: got %+v, %v, want both records' opened facts", cs, err)
	}
	m, err := s.Find(ctx, "api", "", 0)
	if err != nil || len(m) != 1 || m[0].Entity.ID != "id-1" || len(m[0].Entity.Records) != 2 {
		t.Fatalf("Find: got %+v, %v, want the one cluster", m, err)
	}
}

func TestIngestSharedKeyNeverClustersDifferentKinds(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "a", obs("o1", 0, "Component", "a:x/1"))
	ingestOK(t, s, "a", obs("o2", 1, "Team", "a:x/2"))
	ingestOK(t, s, "a", obs("o3", 2, "Team", "a:x/2", aliases("a:x/1")))
	if ls := links(t, s, graph.LinkFilter{}); len(ls) != 1 {
		t.Fatalf("got %d links, want the shared key recorded", len(ls))
	}
	wantClusters(t, s, map[string]graph.EntityID{"a:x/1": "id-1", "a:x/2": "id-2"})
}

func TestIngestLinkedIDsAreAssertedOnlyWhenAuthoritative(t *testing.T) {
	s := ingestStore(t)
	ingestReq(t, s, graph.IngestRequest{Source: "dir", Authoritative: vouchesForNodes, Observations: []*eventv1alpha1.Observation{
		obs("o1", 0, "Person", "dir:user/jane", linked("gh:user_node/1", "gh:user/jane")), // id-1; kindless id-2, id-4 (links take IDs too)
		obs("o2", 0, "Person", "dir:user/tom", linked("gh:user/tom")),                     // id-6; kindless id-7
	}})
	ingestOK(t, s, "gh",
		obs("o3", 1, "Person", "gh:user_node/1", aliases("gh:user/jane"), attrs("name", "Jane")),
		obs("o4", 1, "Person", "gh:user_node/2", aliases("gh:user/tom"), attrs("name", "Tom")))

	wantClusters(t, s, map[string]graph.EntityID{
		"dir:user/jane": "id-1", "gh:user_node/1": "id-1", "gh:user/jane": "id-1",
		"dir:user/tom": "id-6", "gh:user/tom": "id-7", "gh:user_node/2": "id-7",
	})
	var got []string
	for _, l := range links(t, s, graph.LinkFilter{}) {
		got = append(got, string(l.A)+" "+string(l.B)+" "+string(l.Status)+" "+l.Rule)
	}
	want := []string{
		"id-1 id-2 asserted linked_ids", // the node ID the directory vouches for
		"id-1 id-4 proposed linked_ids", // a login it does not
		"id-6 id-7 proposed linked_ids",
		"id-2 id-4 asserted shared_key", // GitHub names both keys
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got links\n%q\nwant\n%q", got, want)
	}
	if ls := links(t, s, graph.LinkFilter{Entity: "dir:user/tom", Status: graph.Asserted}); len(ls) != 0 {
		t.Errorf("got %+v, want no asserted link for Tom", ls)
	}
	// A nil Authoritative vouches for nothing.
	ingestOK(t, s, "dir", obs("o5", 2, "Person", "dir:user/lena", linked("gh:user_node/9")))
	if ls := links(t, s, graph.LinkFilter{Entity: "dir:user/lena"}); len(ls) != 1 || ls[0].Status != graph.Proposed {
		t.Errorf("got %+v, want one proposed link", ls)
	}
}

func TestLinkClustersThreeRecordsAndClosingALinkSplitsThem(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "src",
		obs("oa", 0, "Person", "p:a/1", attrs("name", "a", "tier", "1")),
		obs("ob", 1, "Person", "p:b/1", attrs("name", "b", "tier", "2")),
		obs("oc", 2, "Person", "p:c/1", attrs("name", "c", "tier", "3"))) // records id-1, id-3, id-5
	r := link(t, s, sameAs("id-1", "id-3", graph.Asserted, "rule/x"), sameAs("id-5", "id-3", graph.Asserted, "rule/x"))
	if r.Opened != 2 {
		t.Fatalf("got %+v, want two opened", r)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-3": "id-1", "id-5": "id-1"})
	facts, err := s.Facts(t.Context(), graph.FactFilter{Subject: "id-5", Predicate: "tier"})
	if err != nil || len(facts) != 3 {
		t.Fatalf("got %v, %v, want the three records' facts", facts, err)
	}

	first := links(t, s, graph.LinkFilter{Entity: "id-1"})[0] // id-1 id-3
	if err := s.CloseLink(t.Context(), first.ID, at(60)); err != nil {
		t.Fatal(err)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-3": "id-3", "id-5": "id-3"})
	if facts, _ := s.Facts(t.Context(), graph.FactFilter{Subject: "id-5", Predicate: "tier"}); len(facts) != 2 {
		t.Errorf("got %d facts after the split, want 2", len(facts))
	}
	if facts, _ := s.Facts(t.Context(), graph.FactFilter{Subject: "id-1", Predicate: "tier"}); len(facts) != 1 {
		t.Errorf("got %d facts on id-1, want its own 1", len(facts))
	}
	all := links(t, s, graph.LinkFilter{WithClosed: true})
	if len(all) != 2 || !all[0].ValidTo.Equal(at(60)) || len(links(t, s, graph.LinkFilter{})) != 1 {
		t.Errorf("got %+v, want the first link closed at 60 and one open", all)
	}
	if err := s.CloseLink(t.Context(), "nope", at(60)); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestLinkNeverClustersDifferentKindsButAKindlessRecordJoinsAny(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "src",
		obs("o1", 0, "Person", "p:a/1"),
		obs("o2", 1, "Team", "t:a/1"),
		obs("o3", 2, "Person", "p:b/1", rel("member_of", "x:kindless/1", false))) // id-1, id-2, id-3, kindless id-4
	link(t, s, sameAs("id-1", "id-2", graph.Asserted, "r"), sameAs("id-1", "id-4", graph.Asserted, "r"))
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-2", "id-4": "id-1"})

	// Observed as a Team, the kindless record leaves the Person cluster.
	ingestOK(t, s, "src", obs("o4", 3, "Team", "x:kindless/1"))
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-2", "id-4": "id-4"})
}

func TestLinkDistinctFromBlocksACluster(t *testing.T) {
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1", "p:c/1") // id-1, id-2, id-3
	apart := graph.Proposal{A: "id-3", B: "id-1", Kind: graph.DistinctFrom, Status: graph.Asserted, Rule: "person"}
	link(t, s, apart, sameAs("id-1", "id-2", graph.Asserted, "r"), sameAs("id-2", "id-3", graph.Asserted, "r"))
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-1", "id-3": "id-3"})

	d := links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom})
	if len(d) != 1 || d[0].A != "id-1" || d[0].B != "id-3" {
		t.Fatalf("got %+v, want one distinct_from with the pair ordered", d)
	}
	if err := s.CloseLink(t.Context(), d[0].ID, at(60)); err != nil {
		t.Fatal(err)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-1", "id-3": "id-1"})
}

func TestLinkAssertedPromotesAnOpenProposalAndProposedNeverDemotes(t *testing.T) {
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1") // id-1, id-2
	p := sameAs("id-2", "id-1", graph.Proposed, "matcher/transparent")
	p.Deposit.Features = map[string]float64{"name": 0.9}
	if r := link(t, s, p); r.Opened != 1 {
		t.Fatalf("got %+v, want one opened", r)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-2"})

	if r := link(t, s, sameAs("id-1", "id-2", graph.Asserted, "rule/teams")); r.Opened != 0 || r.Reinforced != 1 {
		t.Fatalf("got %+v, want the open link reinforced, not a second one", r)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-1"})
	ls := links(t, s, graph.LinkFilter{})
	if len(ls) != 1 || ls[0].Status != graph.Asserted || ls[0].Rule != "rule/teams" || len(ls[0].Deposits) != 2 ||
		ls[0].Deposits[0].Features["name"] != 0.9 {
		t.Fatalf("got %+v, want one asserted link by rule/teams keeping the matcher's deposit", ls)
	}

	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent"))
	ls = links(t, s, graph.LinkFilter{})
	if len(ls) != 1 || ls[0].Status != graph.Asserted || ls[0].Rule != "rule/teams" || len(ls[0].Deposits) != 3 {
		t.Fatalf("got %+v, want still asserted by rule/teams with a third deposit", ls)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-1"})
}

func TestLinkRefusesBadProposalsAndWritesNothing(t *testing.T) {
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	good := sameAs("id-1", "id-2", graph.Asserted, "r")
	tests := []struct {
		name string
		p    graph.Proposal
		is   error
	}{
		{"unknown record", sameAs("id-1", "nope", graph.Asserted, "r"), graph.ErrNotFound},
		{"self", sameAs("id-1", "id-1", graph.Asserted, "r"), nil},
		{"kind", graph.Proposal{A: "id-1", B: "id-2", Kind: "alike", Status: graph.Asserted}, nil},
		{"status", graph.Proposal{A: "id-1", B: "id-2", Kind: graph.SameAs, Status: "maybe"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Link(t.Context(), []graph.Proposal{good, tt.p})
			if err == nil || (tt.is != nil && !errors.Is(err, tt.is)) {
				t.Fatalf("got %v, want an error (%v)", err, tt.is)
			}
			if n := count(t, s, `SELECT count(*) FROM identity_link`) + count(t, s, `SELECT count(*) FROM link_deposit`); n != 0 {
				t.Fatalf("got %d rows, want none", n)
			}
		})
	}
}

func TestIngestCompleteSyncClosesAVanishedRecordsFacts(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh",
		obs("o1", 0, "Repository", "k:r/keep", attrs("language", "Go"), rel("depends_on", "k:r/gone", false)),
		obs("o2", 0, "Repository", "k:r/gone", attrs("language", "Go"), rel("owned_by", "k:team/t", false)))
	ingestOK(t, s, "other", obs("o3", 1, "Repository", "k:r/gone", attrs("tier", "1")))
	r := ingestReq(t, s, graph.IngestRequest{
		Source: "gh", Complete: []model.Kind{"Repository"}, SyncStarted: at(10),
		Observations: []*eventv1alpha1.Observation{
			obs("o4", 10, "Repository", "k:r/keep", rel("depends_on", "k:r/gone", false)),
		},
	})
	if r.Closed != 2 {
		t.Fatalf("got %+v, want two closed", r)
	}
	wantFacts(t, s,
		`gone language "Go" [0,10) x1`,
		`gone owned_by t [0,10) x1`,
		`gone tier "1" [1,open) x1`,        // another source's
		`keep depends_on gone [0,open) x2`, // observed again in this sync
		`keep language "Go" [0,open) x1`,
	)
	// Another kind, or no Complete, closes nothing.
	r = ingestReq(t, s, graph.IngestRequest{Source: "gh", Complete: []model.Kind{"Team"}, SyncStarted: at(20)})
	if r.Closed != 0 {
		t.Errorf("got %+v, want nothing closed for Team", r)
	}
}

func TestRecordsListsRecordsOfAKindWithTheirSources(t *testing.T) {
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Person", "p:a/1", aliases("p:a-login/x"), rel("member_of", "t:x/1", false)))
	ingestOK(t, s, "dir", obs("o2", 1, "Person", "p:a-login/x"))
	got, err := s.Records(t.Context(), "Person")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "id-1" || got[0].Cluster != "id-1" ||
		!slices.Equal(got[0].Keys, []model.Key{"p:a-login/x", "p:a/1"}) || !slices.Equal(got[0].Sources, []string{"dir", "gh"}) {
		t.Fatalf("got %+v, want id-1 with both keys and sources", got)
	}
	all, err := s.Records(t.Context(), "")
	if err != nil || len(all) != 2 || all[1].Kind != "" {
		t.Fatalf("got %+v, %v, want the kindless team too", all, err)
	}
}

func TestOpenMovesAFileWithMergedIntoToClusters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE entity (id VARCHAR PRIMARY KEY, kind VARCHAR NOT NULL,
		name VARCHAR NOT NULL, merged_into VARCHAR, created_at TIMESTAMPTZ NOT NULL);
		INSERT INTO entity VALUES ('e1', 'Team', 'payments', NULL, now())`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, Config{Path: path})
	if got := clusterOf(t, s, "e1"); got != "e1" {
		t.Errorf("got cluster %s, want e1", got)
	}
	if _, err := s.db.ExecContext(ctx, `SELECT merged_into FROM entity`); err == nil {
		t.Error("got merged_into still there, want it dropped")
	}
}

func TestReadsHideARelationWhoseEndsJoinedIntoOneCluster(t *testing.T) {
	ctx := t.Context()
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "gh:repo/api", rel("depends_on", "k:a/db", false))) // id-1, id-2, fact id-3
	ingestOK(t, s, "k", obs("o2", 1, "Component", "k:a/db"))
	counts := func() (facts, walked, changes int) {
		t.Helper()
		fs, err := s.Facts(ctx, graph.FactFilter{Predicate: "depends_on"})
		if err != nil {
			t.Fatal(err)
		}
		ws, err := s.Walk(ctx, graph.WalkRequest{From: "id-1", Predicates: []string{"depends_on"}})
		if err != nil {
			t.Fatal(err)
		}
		cs, err := s.Changes(ctx, graph.ChangesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return len(fs), len(ws), len(cs)
	}
	if f, w, c := counts(); f != 1 || w != 1 || c != 1 {
		t.Fatalf("before the join: got %d facts, %d steps, %d changes, want 1 of each", f, w, c)
	}
	link(t, s, sameAs("id-1", "id-2", graph.Asserted, "rule/same"))
	if f, w, c := counts(); f != 0 || w != 0 || c != 0 {
		t.Errorf("after the join: got %d facts, %d steps, %d changes, want none: the fact now points at its own cluster", f, w, c)
	}
}
