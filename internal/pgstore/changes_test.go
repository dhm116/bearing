package pgstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
)

// A listing of the whole graph's changes reads the fact rows once and loads
// timelines only for the newest candidates, however many teams the graph
// holds and however many of them the window touches.
// day is the i-th day of 2026.
func day(i int) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i) }

// nameTeams mints n teams and gives team i the name team-i from when(i).
func nameTeams(t *testing.T, s *Store, clk *testkit.FakeClock, n int, when func(int) time.Time) {
	t.Helper()
	ctx := context.Background()
	ids := mintTeams(t, s, "mint", n)
	for from := 0; from < n; from += 100 {
		cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("names-%d", from)}
		for i := from; i < min(from+100, n); i++ {
			name := &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue(fmt.Sprintf("team-%d", i))}
			cs.Facts = append(cs.Facts, &modelv1alpha1.FactTimeline{SubjectId: ids[i], Predicate: "name", Object: name, Spans: []*modelv1alpha1.FactSpan{{
				Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE,
				ConfidencePpm: 1_000_000, ValidFrom: timestamppb.New(when(i)),
			}}})
		}
		head, _ := s.Head(ctx)
		cs.BaseRecordedAt = timestamppb.New(head)
		clk.Advance(time.Second)
		if _, err := s.Apply(ctx, cs); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChangesPageLoadsOnlyTheNewestCandidates(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	s.changesBatch, s.changesHeld = 2, 8
	const teams = 300
	nameTeams(t, s, clk, teams, day)
	clk.Set(day(teams))

	// withCounts returns a store that counts the scans of fact rows and the
	// batches of timelines loaded.
	var scans, batches int
	counting := withPool(s, &faultPool{before: func(_ context.Context, sql string, _ pgx.Tx) {
		switch {
		case strings.HasPrefix(sql, "SELECT v.kid, v.rec, v.ret, v.data FROM version v"):
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

// scanCounter returns a store that counts, in the variables it is given, the
// scans of fact rows, the batches of timelines loaded and the loads of every
// series of a table (the read a filter that names nothing falls to).
func scanCounter(s *Store, scans, batches, everything *int) *Store {
	return withPool(s, &faultPool{before: func(_ context.Context, sql string, _ pgx.Tx) {
		switch {
		case strings.HasPrefix(sql, "SELECT v.kid, v.rec, v.ret, v.data FROM version v"):
			*scans++
		case strings.HasPrefix(sql, "SELECT DISTINCT subject FROM series_subject"):
			*batches++
		case strings.HasSuffix(sql, "FROM series s WHERE s.tbl = $1"):
			*everything++
		}
	}})
}

// A filter whose object holds only attributes names no subject, so it is no
// reason to load every series: the listing scans, loads batches and filters
// each batch.
func TestChangesPageWithAnAttributeObjectLoadsNoWholeTable(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	s.changesBatch, s.changesHeld = 4, 16
	nameTeams(t, s, clk, 40, day)
	clk.Set(day(60))
	var scans, batches, everything int
	counting := scanCounter(s, &scans, &batches, &everything)
	team7 := &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue("team-7")}
	f := contracts.FactFilter{Object: team7}
	r := contracts.ChangesRequest{Filter: f, T1: day(-1), T2: day(50), Axis: contracts.AxisValid}

	page, err := counting.ChangesPage(ctx, r)
	if err != nil || len(page.Changes) != 1 || !page.Changes[0].GetChangedAt().AsTime().Equal(day(7)) {
		t.Fatalf("got %v, %v, want the one change on day 7", page.Changes, err)
	}
	if everything != 0 || scans < 1 {
		t.Errorf("%d loads of a whole table and %d scans, want none and at least one", everything, scans)
	}
	everything, scans = 0, 0
	f.Predicate = "name"
	at, err := counting.LastChange(ctx, f, day(50), contracts.AxisValid)
	if err != nil || !at.Equal(day(7)) || everything != 0 || scans < 1 {
		t.Errorf("last change: got %v, %v with %d loads of a whole table and %d scans, want day 7, none and at least one", at, err, everything, scans)
	}
}

// Facts that all changed at one instant cannot be told apart by when, so the
// page is settled by every one of them; the listing still answers, a held
// scan at a time, and a cursor at the window's start costs nothing.
func TestChangesPageOfFactsChangedAtOneInstant(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	s.changesBatch, s.changesHeld = 5, 12
	const teams = 40
	nameTeams(t, s, clk, teams, func(int) time.Time { return day(10) })
	clk.Set(day(60))
	var scans, batches, everything int
	counting := scanCounter(s, &scans, &batches, &everything)
	r := contracts.ChangesRequest{T1: day(0), T2: day(50), Axis: contracts.AxisValid, Limit: 3}

	var seen []string
	for {
		scans, batches = 0, 0
		page, err := counting.ChangesPage(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		// Every change shares the instant, so each page is settled by all of
		// them: at most one scan for each held block of the timelines.
		if want := (teams + 11) / 12; scans > want {
			t.Errorf("page %d: %d scans, want at most %d", len(seen)/3, scans, want)
		}
		for _, c := range page.Changes {
			seen = append(seen, c.GetFactId())
		}
		if page.Next == nil {
			break
		}
		r.After, r.T1, r.T2 = page.Next, page.T1, page.T2
	}
	if len(seen) != teams || !slices.IsSorted(seen) {
		t.Fatalf("got %d changes, sorted %v, want %d in fact ID order", len(seen), slices.IsSorted(seen), teams)
	}

	scans, batches = 0, 0
	r.After = &contracts.ChangeCursor{ChangedAt: day(0), FactID: "x"}
	page, err := counting.ChangesPage(ctx, r)
	if err != nil || len(page.Changes) != 0 || page.Next != nil || scans != 0 || batches != 0 {
		t.Errorf("a cursor at the window's start: got %d changes, %d scans, %d batches, %v; want an empty page for free", len(page.Changes), scans, batches, err)
	}
}
