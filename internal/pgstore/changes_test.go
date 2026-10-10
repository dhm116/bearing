package pgstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
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
		case strings.HasPrefix(sql, "SELECT s.head FROM series s WHERE s.tbl = $1 AND s.kid = ANY"):
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
		case strings.HasPrefix(sql, "SELECT s.head FROM series s WHERE s.tbl = $1 AND s.kid = ANY"):
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

// pair is a PostgreSQL store and the reference store given the same
// history, with batches and scans small enough that a few facts span many.
type pair struct {
	t   *testing.T
	pg  *Store
	ref *memstore.Store
	clk *testkit.FakeClock
}

func newPair(t *testing.T) *pair {
	t.Helper()
	pg, clk := graphStoreAt(t)
	pg.changesBatch, pg.changesHeld = 2, 3
	ref := memstore.New()
	ref.Now, ref.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	return &pair{t, pg, ref, clk}
}

// apply gives both stores cs a minute later.
func (p *pair) apply(cs *modelv1alpha1.ChangeSet) contracts.ApplyResult {
	p.t.Helper()
	ctx := context.Background()
	if head, _ := p.ref.Head(ctx); !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	p.clk.Advance(time.Minute)
	res, err := p.ref.Apply(ctx, proto.CloneOf(cs))
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.pg.Apply(ctx, cs); err != nil {
		p.t.Fatal(err)
	}
	return res
}

// teams mints n teams and returns their IDs.
func (p *pair) teams(n int) []string {
	cs := &modelv1alpha1.ChangeSet{EventId: "mint"}
	for i := range n {
		cs.Mints = append(cs.Mints, mint(fmt.Sprintf("new:t%d", i)))
	}
	res := p.apply(cs)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = string(res.Subjects[fmt.Sprintf("new:t%d", i)])
	}
	return ids
}

func mint(ref string) *modelv1alpha1.Mint {
	return &modelv1alpha1.Mint{Ref: ref, Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}
}

// named is a team's name from from until to; a zero time is unbounded.
func named(id, value string, from, to time.Time) *modelv1alpha1.FactTimeline {
	sp := &modelv1alpha1.FactSpan{Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: 1_000_000}
	if !from.IsZero() {
		sp.ValidFrom = timestamppb.New(from)
	}
	if !to.IsZero() {
		sp.ValidTo = timestamppb.New(to)
	}
	return &modelv1alpha1.FactTimeline{
		SubjectId: id, Predicate: "name", Spans: []*modelv1alpha1.FactSpan{sp},
		Object: &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue(value)},
	}
}

// same lists r page by page from both stores, fails unless they agree on
// every page, and returns how many changes there were.
func (p *pair) same(r contracts.ChangesRequest) int {
	p.t.Helper()
	ctx := context.Background()
	pgReq, refReq := r, r
	n := 0
	for page := 1; page < 200; page++ {
		want, err := p.ref.ChangesPage(ctx, refReq)
		if err != nil {
			p.t.Fatal(err)
		}
		got, err := p.pg.ChangesPage(ctx, pgReq)
		if err != nil {
			p.t.Fatal(err)
		}
		if !slices.Equal(factIDs(got.Changes), factIDs(want.Changes)) || fmt.Sprint(got.Next) != fmt.Sprint(want.Next) {
			p.t.Fatalf("%+v page %d: got %v next %v, want %v next %v", r, page, factIDs(got.Changes), got.Next, factIDs(want.Changes), want.Next)
		}
		n += len(want.Changes)
		if want.Next == nil {
			return n
		}
		refReq.T1, refReq.T2, refReq.After = want.T1, want.T2, want.Next
		pgReq.T1, pgReq.T2, pgReq.After = got.T1, got.T2, got.Next
	}
	p.t.Fatal("a listing never ends")
	return n
}

func factIDs(cs []*modelv1alpha1.FactChange) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.GetFactId()
	}
	return out
}

// A fact whose only boundary in the window is its end is a candidate: it
// stopped being true, which is the change a membership that ended is.
func TestChangesPageListsFactsThatEndedInTheWindow(t *testing.T) {
	p := newPair(t)
	ids := p.teams(6)
	cs := &modelv1alpha1.ChangeSet{EventId: "names", Facts: []*modelv1alpha1.FactTimeline{named(ids[0], "old", day(0), day(10))}}
	for i := 1; i < 6; i++ {
		cs.Facts = append(cs.Facts, named(ids[i], fmt.Sprint(i), day(11+i), time.Time{}))
	}
	p.apply(cs)
	p.clk.Set(day(30))
	if n := p.same(contracts.ChangesRequest{T1: day(5), T2: day(20), Axis: contracts.AxisValid, Limit: 2}); n != 6 {
		t.Fatalf("listed %d changes, want the five starts and the one end", n)
	}
}

// A timeline replaced by one with no spans is a change on the record axis
// that no valid-time boundary marks.
func TestChangesPageListsRetractionsOnTheRecordAxis(t *testing.T) {
	ctx := context.Background()
	p := newPair(t)
	ids := p.teams(4)
	cs := &modelv1alpha1.ChangeSet{EventId: "names"}
	for i, id := range ids {
		cs.Facts = append(cs.Facts, named(id, fmt.Sprint(i), day(0), time.Time{}))
	}
	p.apply(cs)
	before, _ := p.ref.Head(ctx)
	retraction := named(ids[0], "0", time.Time{}, time.Time{})
	retraction.Spans = nil
	p.apply(&modelv1alpha1.ChangeSet{EventId: "retract", Facts: []*modelv1alpha1.FactTimeline{retraction}})
	after, _ := p.ref.Head(ctx)
	p.clk.Set(day(30))
	if n := p.same(contracts.ChangesRequest{T1: before, T2: after, Axis: contracts.AxisRecord, Limit: 2}); n != 1 {
		t.Fatalf("listed %d changes, want the retraction", n)
	}
}

// Facts that changed at one instant are listed in fact ID order with none
// dropped at the edge of a page, whatever the page size.
func TestChangesPageKeepsTiesAtThePageEdge(t *testing.T) {
	p := newPair(t)
	ids := p.teams(9)
	cs := &modelv1alpha1.ChangeSet{EventId: "names"}
	for i, id := range ids {
		cs.Facts = append(cs.Facts, named(id, fmt.Sprint(i), day(10), time.Time{}))
	}
	p.apply(cs)
	p.clk.Set(day(30))
	for limit := 1; limit <= 5; limit++ {
		if n := p.same(contracts.ChangesRequest{T1: day(5), T2: day(20), Axis: contracts.AxisValid, Limit: limit}); n != 9 {
			t.Fatalf("limit %d: listed %d changes, want 9", limit, n)
		}
	}
}

// A subject that many facts point at is not loaded for the facts about one
// of the things that point at it.
func TestChangesPageBatchDoesNotLoadWhatPointsAtASharedSubject(t *testing.T) {
	ctx := context.Background()
	p := newPair(t)
	ids := p.teams(41)
	hub := ids[0]
	cs := &modelv1alpha1.ChangeSet{EventId: "owners"}
	for i := 1; i < 41; i++ {
		ft := named(ids[i], "", day(i), time.Time{})
		ft.Predicate, ft.Object = "owned_by", &modelv1alpha1.FactObject{SubjectId: hub}
		cs.Facts = append(cs.Facts, ft)
		cs.Supports = append(cs.Supports, &modelv1alpha1.SupportTimeline{
			Source: "github-acme", SubjectId: ids[i], Predicate: "owned_by", Object: ft.Object,
			Versions: []*modelv1alpha1.Support{{Source: "github-acme", ConfidencePpm: proto.Uint32(1_000_000), Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT, EventId: "x", ObservedAt: timestamppb.New(p.clk.Now()), ValidFrom: timestamppb.New(day(i))}},
		})
	}
	p.apply(cs)
	p.clk.Set(day(100))
	if n := p.same(contracts.ChangesRequest{T1: day(0), T2: day(60), Axis: contracts.AxisValid, Limit: 7}); n != 40 {
		t.Fatalf("listed %d changes, want 40", n)
	}
	// The batch for two of the owned teams holds their facts and no other.
	var loaded int
	err := p.pg.readTx(ctx, func(q querier) error {
		meta, err := readMeta(ctx, q, false)
		if err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT kid FROM series WHERE tbl = $1 AND predicate = $2 ORDER BY key LIMIT 2`, tbl(memstore.TableFacts), []byte("owned_by"))
		if err != nil {
			return err
		}
		kids, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		m, err := p.pg.loadBatch(ctx, q, meta, []candidate{{kid: kids[0]}, {kid: kids[1]}})
		if err != nil {
			return err
		}
		states, err := m.AsOf(ctx, contracts.FactFilter{}, day(100), time.Time{})
		loaded = len(states)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 2 {
		t.Errorf("a batch of 2 facts loaded %d facts, want 2: the shared subject drags in everything pointing at it", loaded)
	}
}
