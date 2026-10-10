package pgstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// A listing of the whole graph's changes reads the fact rows once and loads
// timelines only for the newest candidates, however many teams the graph
// holds and however many of them the window touches.
func TestChangesPageLoadsOnlyTheNewestCandidates(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	s.changesBatch, s.changesHeld = 2, 8
	const teams = 300
	ids := mintTeams(t, s, "mint", teams)
	day := func(i int) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i) }
	// Team i takes its name on day i, and the clock is at day 300.
	for from := 0; from < teams; from += 100 {
		cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("names-%d", from)}
		for i := from; i < from+100; i++ {
			name := &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue(fmt.Sprintf("team-%d", i))}
			cs.Facts = append(cs.Facts, &modelv1alpha1.FactTimeline{SubjectId: ids[i], Predicate: "name", Object: name, Spans: []*modelv1alpha1.FactSpan{{
				Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE,
				ConfidencePpm: 1_000_000, ValidFrom: timestamppb.New(day(i)),
			}}})
		}
		head, _ := s.Head(ctx)
		cs.BaseRecordedAt = timestamppb.New(head)
		clk.Advance(time.Second)
		if _, err := s.Apply(ctx, cs); err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(day(teams))

	// withCounts returns a store that counts the scans of fact rows and the
	// batches of timelines loaded.
	var scans, batches int
	counting := withPool(s, &faultPool{before: func(_ context.Context, sql string, _ pgx.Tx) {
		switch {
		case strings.HasSuffix(sql, "WHERE v.tbl = $1 ORDER BY v.kid, v.n"):
			scans++
		case strings.HasPrefix(sql, "SELECT DISTINCT subject FROM series_subject"):
			batches++
		}
	}})
	listing := func(r contracts.ChangesRequest) contracts.ChangesPage {
		t.Helper()
		scans, batches = 0, 0
		page, err := counting.ChangesPage(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	window := contracts.ChangesRequest{T1: day(260).Add(-time.Hour), T2: day(300), Axis: contracts.AxisValid}

	// Forty changes in the window, a page of three: the newest two batches
	// settle it, not the twenty that the window holds.
	window.Limit = 3
	page := listing(window)
	if len(page.Changes) != 3 || page.Next == nil || !page.Changes[0].GetChangedAt().AsTime().Equal(day(299)) {
		t.Fatalf("got %d changes %v, want 3 from day 299 and a cursor", len(page.Changes), page.Next)
	}
	if scans != 1 || batches < 1 || batches > 3 {
		t.Errorf("a page of 3 from 40 changes: %d scans and %d batches, want 1 scan and at most 3 batches", scans, batches)
	}
	// Nothing in the window: one scan, nothing loaded.
	empty := window
	empty.T1, empty.T2 = day(400), day(401)
	if page = listing(empty); len(page.Changes) != 0 || scans != 1 || batches != 0 {
		t.Errorf("an empty window: %d changes, %d scans, %d batches, want none, 1 and 0", len(page.Changes), scans, batches)
	}
	// Every change in the window, read in pages, is read from batches of the
	// candidates the window holds and no others.
	window.Limit = contracts.MaxChangesLimit
	if page = listing(window); len(page.Changes) != 40 || batches != 20 {
		t.Errorf("all 40 changes: got %d, from %d batches, want 40 from 20", len(page.Changes), batches)
	}
	// The newest change at or before day 100 is found by the first batch.
	scans, batches = 0, 0
	at, err := counting.LastChange(ctx, contracts.FactFilter{}, day(100), contracts.AxisValid)
	if err != nil || !at.Equal(day(100)) || scans != 1 || batches != 1 {
		t.Errorf("last change at day 100: got %v, %v with %d scans and %d batches, want day 100, 1 and 1", at, err, scans, batches)
	}
}
