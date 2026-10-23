package duckstore

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

func acme(t *testing.T) *Store {
	t.Helper()
	s := newStore(t, Config{})
	loadAcme(t, s)
	return s
}

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func factIDs(fs []graph.Fact) []string {
	var ids []string
	for _, f := range fs {
		ids = append(ids, f.ID)
	}
	return ids
}

func TestEntityResolvesIDsKeysAndRecordsToTheCluster(t *testing.T) {
	t.Parallel()
	s := acme(t)
	for _, ref := range []string{"s-api", "svc:api", "svc:api-old", "s-old", "svc:legacy-api"} {
		t.Run(ref, func(t *testing.T) {
			e, err := s.Entity(t.Context(), ref)
			if err != nil {
				t.Fatal(err)
			}
			wantKeys := []model.Key{"svc:api", "svc:api-old", "svc:legacy-api"}
			if e.ID != "s-api" || e.Kind != "Service" || e.Name != "api" || !slices.Equal(e.Keys, wantKeys) || len(e.Records) != 2 {
				t.Fatalf("got %+v, want s-api with keys %v", e, wantKeys)
			}
		})
	}
	if _, err := s.Entity(t.Context(), "nope"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestFindRanksExactSubstringThenFuzzy(t *testing.T) {
	t.Parallel()
	s := acme(t)
	tests := []struct {
		name  string
		query string
		kind  model.Kind
		limit int
		want  []graph.EntityID
	}{
		{"exact name, second record folded into its cluster", "API", "", 0, []graph.EntityID{"s-api"}},
		{"substring of a key", "payments", "", 0, []graph.EntityID{"t-pay"}},
		{"fuzzy", "ledgr", "", 0, []graph.EntityID{"s-ledger"}},
		{"kind filter", "alice", "Service", 0, nil},
		{"kind match", "alice", "Person", 0, []graph.EntityID{"p-alice"}},
		{"limit", "ledger", "", 1, []graph.EntityID{"s-ledger"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Find(t.Context(), tt.query, tt.kind, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			var ids []graph.EntityID
			for _, m := range got {
				ids = append(ids, m.Entity.ID)
			}
			if !slices.Equal(ids, tt.want) {
				t.Fatalf("got %v, want %v", ids, tt.want)
			}
		})
	}
	got, _ := s.Find(t.Context(), "ledger", "", 0)
	if got[0].Score != 1 || len(got[0].Entity.Keys) != 1 {
		t.Fatalf("got %+v, want score 1 with its key", got[0])
	}
}

func TestFactsFilterAndTimeTravel(t *testing.T) {
	t.Parallel()
	s := acme(t)
	tests := []struct {
		name string
		f    graph.FactFilter
		want []string
	}{
		{"subject now", graph.FactFilter{Subject: "s-api"}, []string{"f01", "f08", "f04", "f10"}},
		{"by another record of the cluster", graph.FactFilter{Subject: "s-old", Predicate: "version"}, []string{"f10"}},
		{"past version", graph.FactFilter{Subject: "s-api", Predicate: "version", At: day(3, 1)}, []string{"f09"}},
		{"closed boundary", graph.FactFilter{Subject: "s-api", Predicate: "version", At: day(6, 1)}, []string{"f10"}},
		{"object", graph.FactFilter{Object: "t-pay", Predicate: "owned_by"}, []string{"f04", "f05"}},
		{"closed fact hidden now", graph.FactFilter{Subject: "s-db", Predicate: "owned_by"}, nil},
		{"closed fact in the past", graph.FactFilter{Subject: "s-db", Predicate: "owned_by", At: day(2, 1)}, []string{"f06"}},
		{"proposed with flag", graph.FactFilter{Subject: "s-db", Predicate: "owned_by", WithProposed: true}, []string{"f07"}},
		{"before anything", graph.FactFilter{Subject: "s-api", At: day(1, 1).Add(-time.Hour)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Facts(t.Context(), tt.f)
			if err != nil {
				t.Fatal(err)
			}
			if ids := factIDs(got); !slices.Equal(ids, tt.want) {
				t.Fatalf("got %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestFactsCarryValuesAndSources(t *testing.T) {
	t.Parallel()
	s := acme(t)
	got, err := s.Facts(t.Context(), graph.FactFilter{Subject: "s-api"})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]graph.Fact{}
	for _, f := range got {
		byID[f.ID] = f
	}
	if v := byID["f08"].Value; v != "go" {
		t.Fatalf("got value %v, want go", v)
	}
	f01 := byID["f01"]
	if f01.Object != "s-ledger" || f01.Value != nil || !f01.ValidTo.IsZero() || f01.Status != graph.Asserted {
		t.Fatalf("got %+v, want an open asserted relation", f01)
	}
	want := []graph.Source{
		{ObservationID: "o1", Source: "gh", ObservedAt: day(1, 1), URL: "https://example.test/o1"},
		{ObservationID: "o3", Source: "gh", ObservedAt: day(6, 1)},
	}
	if !slices.Equal(f01.Sources, want) {
		t.Fatalf("got sources %+v, want %+v", f01.Sources, want)
	}
	closed, _ := s.Facts(t.Context(), graph.FactFilter{Subject: "s-db", Predicate: "owned_by", At: day(2, 1)})
	if len(closed) == 0 || !closed[0].ValidTo.Equal(day(3, 1)) {
		t.Fatalf("got %+v, want the closed fact with valid_to 2026-03-01", closed)
	}
}

func TestWalkFollowsEdgesOnceAndEndsCycles(t *testing.T) {
	t.Parallel()
	s := acme(t)
	type step struct {
		depth int
		id    string
	}
	tests := []struct {
		name string
		r    graph.WalkRequest
		want []step
	}{
		{"out, cycle ends", graph.WalkRequest{From: "s-api", Predicates: []string{"depends_on"}}, []step{{1, "f01"}, {2, "f02"}}},
		{"max depth", graph.WalkRequest{From: "s-api", Predicates: []string{"depends_on"}, MaxDepth: 1}, []step{{1, "f01"}}},
		{"in", graph.WalkRequest{From: "s-ledger", Predicates: []string{"depends_on"}, Direction: graph.In}, []step{{1, "f01"}, {1, "f03"}}},
		{"start at another record", graph.WalkRequest{From: "s-old", Predicates: []string{"depends_on"}, MaxDepth: 1}, []step{{1, "f01"}}},
		{
			"every relation, proposed and closed hidden",
			graph.WalkRequest{From: "t-pay", Direction: graph.In, MaxDepth: 1},
			[]step{{1, "f04"}, {1, "f05"}, {1, "f12"}, {1, "f13"}},
		},
		{"past", graph.WalkRequest{From: "s-db", Predicates: []string{"owned_by"}, At: day(2, 1)}, []step{{1, "f06"}}},
		{"no match", graph.WalkRequest{From: "s-api", Predicates: []string{"nope"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Walk(t.Context(), tt.r)
			if err != nil {
				t.Fatal(err)
			}
			var steps []step
			for _, g := range got {
				steps = append(steps, step{g.Depth, g.Fact.ID})
			}
			if !slices.Equal(steps, tt.want) {
				t.Fatalf("got %v, want %v", steps, tt.want)
			}
		})
	}
	got, _ := s.Walk(t.Context(), graph.WalkRequest{From: "s-api", Predicates: []string{"depends_on"}})
	if len(got[0].Fact.Sources) != 2 || len(got[1].Fact.Sources) != 1 {
		t.Fatalf("got sources %+v, want 2 and 1", got)
	}
}

func TestChangesListsOpenedAndClosedNewestFirst(t *testing.T) {
	t.Parallel()
	s := acme(t)
	type change struct {
		kind graph.ChangeKind
		id   string
		at   time.Time
	}
	tests := []struct {
		name string
		r    graph.ChangesRequest
		want []change
	}{
		{"window, proposed hidden", graph.ChangesRequest{Since: day(2, 1), Until: day(6, 2)}, []change{
			{graph.Closed, "f09", day(6, 1)},
			{graph.Opened, "f10", day(6, 1)},
			{graph.Closed, "f06", day(3, 1)},
			{graph.Opened, "f13", day(2, 1)},
		}},
		{"until is exclusive", graph.ChangesRequest{Since: day(2, 1), Until: day(6, 1)}, []change{
			{graph.Closed, "f06", day(3, 1)}, {graph.Opened, "f13", day(2, 1)},
		}},
		{"subject as object", graph.ChangesRequest{Subject: "t-pay", Since: day(2, 1), Until: day(6, 2)}, []change{
			{graph.Closed, "f06", day(3, 1)}, {graph.Opened, "f13", day(2, 1)},
		}},
		{"subject as subject, by another record of the cluster", graph.ChangesRequest{Subject: "s-old", Since: day(5, 1), Until: day(7, 1)}, []change{
			{graph.Closed, "f09", day(6, 1)}, {graph.Opened, "f10", day(6, 1)},
		}},
		{"limit", graph.ChangesRequest{Since: day(2, 1), Until: day(6, 2), Limit: 1}, []change{{graph.Closed, "f09", day(6, 1)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Changes(t.Context(), tt.r)
			if err != nil {
				t.Fatal(err)
			}
			var cs []change
			for _, c := range got {
				cs = append(cs, change{c.Kind, c.Fact.ID, c.At})
			}
			if !slices.EqualFunc(cs, tt.want, func(a, b change) bool {
				return a.kind == b.kind && a.id == b.id && a.at.Equal(b.at)
			}) {
				t.Fatalf("got %v, want %v", cs, tt.want)
			}
		})
	}
	got, _ := s.Changes(t.Context(), graph.ChangesRequest{Subject: "s-api", Since: day(6, 1), Until: day(6, 2)})
	if len(got) == 0 || len(got[0].Fact.Sources) == 0 {
		t.Fatalf("got %+v, want changes with sources", got)
	}
}

func TestFactsReturnFirstAndLatestSourcePerSourceWithTheCount(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	for i := range 100 {
		ingestOK(t, s, "gh", obs(fmt.Sprintf("o%03d", i), i, "Component", "k:a/api", rel("depends_on", "k:a/db", false)))
	}
	facts, err := s.Facts(t.Context(), graph.FactFilter{Predicate: "depends_on"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	f := facts[0]
	if f.SourceCount != 100 || len(f.Sources) != 2 || f.Sources[0].ObservationID != "o000" || f.Sources[1].ObservationID != "o099" {
		t.Fatalf("got count %d, sources %+v; want 100, o000 then o099", f.SourceCount, f.Sources)
	}
}

func TestEntityNeverMissesARecordWhileItsClusterChanges(t *testing.T) {
	t.Parallel()
	// Each read runs in one transaction, so a recluster cannot land between
	// resolving a ref to its cluster and loading that cluster.
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1") // id-1, id-2
	ctx := t.Context()
	done := make(chan error, 1)
	go func() {
		defer close(done)
		for range 50 {
			if _, err := s.Link(ctx, []graph.Proposal{sameAs("id-1", "id-2", graph.Asserted, "test")}); err != nil {
				done <- err
				return
			}
			ls, err := s.Links(ctx, graph.LinkFilter{Entity: "id-2", Kind: graph.SameAs})
			if err != nil || len(ls) != 1 {
				done <- fmt.Errorf("links %v: %w", ls, err)
				return
			}
			if err := s.CloseLink(ctx, ls[0].ID, at(60)); err != nil {
				done <- err
				return
			}
			if _, err := s.Records(ctx, ""); err != nil { // gives the reader turns while apart
				done <- err
				return
			}
		}
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
		if _, err := s.Entity(ctx, "p:b/1"); err != nil {
			t.Fatalf("got %v while the cluster changed, want the entity", err)
		}
	}
}
