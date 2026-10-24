package query_test

import (
	"context"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/query"
)

func TestLinksHideProposalsBetweenJoinedRecordsUntilTheJoinCloses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := duckstore.Open(ctx, duckstore.Config{Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Ingest(ctx, graph.IngestRequest{Source: "src", Observations: []*eventv1alpha1.Observation{
		ob("p1", 1, "Person", "gh:user/jane", attr("name", "Jane")),
		ob("p2", 1, "Person", "ad:user/jane", attr("name", "Jane")),
		ob("p3", 1, "Person", "gh:user/tom", attr("name", "Tom")),
		ob("p4", 1, "Person", "hr:user/jane", attr("name", "Jane")),
	}}); err != nil {
		t.Fatal(err)
	}
	recs, err := s.Records(ctx, "Person")
	if err != nil || len(recs) != 4 {
		t.Fatalf("got %v, %v, want four people", recs, err)
	}
	id := map[string]graph.EntityID{}
	for _, r := range recs {
		id[string(r.Keys[0])] = r.ID
	}
	prop := func(a, b string, st graph.Status, rule string) {
		t.Helper()
		if _, err := s.Link(ctx, []graph.Proposal{{A: id[a], B: id[b], Kind: graph.SameAs, Status: st, Rule: rule, Deposit: graph.Deposit{At: at(1), Weight: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	prop("gh:user/jane", "hr:user/jane", graph.Proposed, "matcher")
	prop("gh:user/jane", "gh:user/tom", graph.Proposed, "matcher")
	q := &query.Querier{Graph: s, Now: func() time.Time { return at(100) }}
	queue := func() []query.Link {
		t.Helper()
		ls, err := q.Links(ctx, query.LinksRequest{Status: "proposed"})
		if err != nil {
			t.Fatal(err)
		}
		return ls
	}
	if n := len(queue()); n != 2 {
		t.Fatalf("got %d proposals before any join, want 2", n)
	}
	prop("gh:user/jane", "ad:user/jane", graph.Asserted, "person")
	prop("ad:user/jane", "hr:user/jane", graph.Asserted, "person")
	if ls := queue(); len(ls) != 1 || ls[0].B.Key != "gh:user/tom" && ls[0].A.Key != "gh:user/tom" {
		t.Fatalf("got %+v, want only the proposal that names Tom while Jane's records are joined", ls)
	}
	asserted, err := s.Links(ctx, graph.LinkFilter{Status: graph.Asserted})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range asserted {
		if l.B == id["hr:user/jane"] { // the link that brings the directory record in
			if err := s.CloseLink(ctx, l.ID, at(50)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := len(queue()); n != 2 {
		t.Fatalf("got %d proposals after the join closed, want the hidden one back (2)", n)
	}
}
