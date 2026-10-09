package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
)

func graphStoreAt(t testing.TB) (*Store, *testkit.FakeClock) {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	s := newTestStore(t)
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	return s, clk
}

// reopen opens a second store on s's schema, as another process would, with
// its own clock and ID source.
func reopen(t testing.TB, o Options, clk *testkit.FakeClock) *Store {
	t.Helper()
	again, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close(context.Background()) })
	again.Now, again.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	return again
}

func stateEntries(n int, prefix string) []*modelv1alpha1.StateEntry {
	out := make([]*modelv1alpha1.StateEntry, n)
	for i := range out {
		v, _ := anypb.New(wrapperspb.Int64(int64(i)))
		out[i] = &modelv1alpha1.StateEntry{Key: fmt.Sprintf("%s-%05d", prefix, i), Value: v}
	}
	return out
}

// count runs a query that returns one number.
func count(t testing.TB, s *Store, sql string, args ...any) int {
	t.Helper()
	var n int
	err := s.readTx(context.Background(), func(q querier) error { return q.QueryRow(context.Background(), sql, args...).Scan(&n) })
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mintTeams(t testing.TB, s contracts.GraphStore, event string, n int) []string {
	t.Helper()
	cs := &modelv1alpha1.ChangeSet{EventId: event}
	for i := range n {
		cs.Mints = append(cs.Mints, &modelv1alpha1.Mint{Ref: fmt.Sprintf("new:t%d", i), Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION})
	}
	if head, _ := s.Head(context.Background()); !head.IsZero() {
		cs.BaseRecordedAt = timestamppb.New(head)
	}
	res, err := s.Apply(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = string(res.Subjects[fmt.Sprintf("new:t%d", i)])
	}
	return ids
}

func merge(t testing.TB, s contracts.GraphStore, event string, a, b string) {
	t.Helper()
	head, _ := s.Head(context.Background())
	_, err := s.Apply(context.Background(), &modelv1alpha1.ChangeSet{
		EventId: event, BaseRecordedAt: timestamppb.New(head),
		Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{a, b}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A second store on the same schema reads what the first applied, with the
// refs of a ChangeSet resolved in its facts, and answers a repeated event
// with the original result.
func TestGraphSurvivesReopeningTheDatabase(t *testing.T) {
	ctx := context.Background()
	s, o := openTestStore(t)
	clk := testkit.NewClock(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	object := &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue("payments")}
	cs := &modelv1alpha1.ChangeSet{
		EventId: "reopen",
		Mints:   []*modelv1alpha1.Mint{{Ref: "new:r", Kind: "Repository", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}},
		Bindings: []*modelv1alpha1.BindingTimeline{{Alias: "github:repo_node/R_1", Bindings: []*modelv1alpha1.Binding{
			{Alias: "github:repo_node/R_1", SubjectId: "new:r"},
		}}},
		Supports: []*modelv1alpha1.SupportTimeline{{
			Source: "github-acme", SubjectId: "new:r", Predicate: "name", Object: object,
			Versions: []*modelv1alpha1.Support{{Source: "github-acme", ConfidencePpm: proto.Uint32(1_000_000), Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT, EventId: "reopen", ObservedAt: timestamppb.New(clk.Now())}},
		}},
		Facts: []*modelv1alpha1.FactTimeline{{
			SubjectId: "new:r", Predicate: "name", Object: object,
			Spans: []*modelv1alpha1.FactSpan{{Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: 1_000_000}},
		}},
		State: stateEntries(1, "cursor"),
	}
	res, err := s.Apply(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	id := string(res.Subjects["new:r"])

	other := reopen(t, o, clk)
	if head, err := other.Head(ctx); err != nil || !head.Equal(res.RecordedAt) {
		t.Fatalf("got head %v, %v, want %v", head, err, res.RecordedAt)
	}
	sub, err := other.ResolveKey(ctx, "github:repo_node/R_1", time.Time{}, time.Time{})
	if err != nil || sub.GetSubjectId() != id {
		t.Fatalf("got %v, %v, want subject %s", sub, err, id)
	}
	facts, err := other.AsOf(ctx, contracts.FactFilter{SubjectID: contracts.SubjectID(id)}, time.Time{}, time.Time{})
	if err != nil || len(facts) != 1 || facts[0].GetSubjectId() != id || len(facts[0].GetSupports()) != 1 {
		t.Fatalf("got %v, %v, want one fact on %s with one support", facts, err, id)
	}
	if st, err := other.State(ctx, []string{"cursor-00000"}, time.Time{}); err != nil || len(st) != 1 {
		t.Fatalf("got %v, %v, want the cursor", st, err)
	}
	dup, err := other.Apply(ctx, cs)
	if err != nil || !dup.Duplicate || dup.Subjects["new:r"] != contracts.SubjectID(id) || !dup.RecordedAt.Equal(res.RecordedAt) {
		t.Fatalf("got %+v, %v, want the original result with Duplicate", dup, err)
	}
}

func TestMigrationsAreRecordedInOrderAndNewerDatabasesRefused(t *testing.T) {
	ctx := context.Background()
	s, o := openTestStore(t)
	var got []int
	err := s.readTx(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT version FROM schema_version ORDER BY version`)
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, pgx.RowTo[int])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var want []int
	for _, m := range migrations {
		want = append(want, m.version)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got versions %v, want %v", got, want)
	}
	// Opening again changes nothing.
	again := reopen(t, o, testkit.NewClock(time.Time{}))
	if n := count(t, again, `SELECT count(*) FROM schema_version`); n != len(migrations) {
		t.Fatalf("got %d versions after reopening, want %d", n, len(migrations))
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_version (version, name) VALUES (99, 'from the future')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, o); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("got %v, want a refusal naming version 99", err)
	}
}

// Stores opened at once on one new schema take turns migrating it.
func TestOpeningManyStoresAtOnceMigratesOnce(t *testing.T) {
	ctx := context.Background()
	_, o := openTestStore(t) // the schema exists and is migrated; use a new one beside it
	o.Schema += "_x"
	admin := adminOptions(t)
	t.Cleanup(func() { dropSchema(context.WithoutCancel(ctx), t, admin, o.Schema, false) })
	if scopedTests() {
		t.Skip("the administrator creates the schema")
	}
	var wg sync.WaitGroup
	stores := make([]*Store, 6)
	errs := make([]error, len(stores))
	for i := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = Open(ctx, o)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
		defer func() { _ = stores[i].Close(ctx) }()
	}
	if n := count(t, stores[0], `SELECT count(*) FROM schema_version`); n != len(migrations) {
		t.Fatalf("got %d recorded versions, want %d", n, len(migrations))
	}
}

// A repeated event applies once, however many callers race on it.
func TestConcurrentAppliesOfOneEventApplyOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := graphStoreAt(t)
	cs := &modelv1alpha1.ChangeSet{EventId: "same", State: stateEntries(3, "same")}
	var fresh, dup atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.Apply(ctx, proto.CloneOf(cs))
			switch {
			case err != nil:
				t.Error(err)
			case res.Duplicate:
				dup.Add(1)
			default:
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != 1 || dup.Load() != 7 {
		t.Fatalf("got %d applies and %d duplicates, want 1 and 7", fresh.Load(), dup.Load())
	}
	if n := count(t, s, `SELECT count(*) FROM journal`); n != 1 {
		t.Fatalf("got %d journal entries, want 1", n)
	}
}

func TestApplyRefusesEventIDsThatCannotBeStored(t *testing.T) {
	s, _ := graphStoreAt(t)
	for _, id := range []string{"", "a\x00b"} {
		if _, err := s.Apply(context.Background(), &modelv1alpha1.ChangeSet{EventId: id}); err == nil {
			t.Errorf("applied event %q", id)
		}
	}
}

// An un-merge names the subject it revives by a ref, and the rest of the
// ChangeSet may write claims about that ref. Those land in series that
// already exist, which the apply has to load although the ChangeSet names no
// subject that holds them.
func TestUnmergeWithClaimsAboutTheRevivedSubjectMatchesTheReference(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
	sclk, mclk := testkit.NewClock(start), testkit.NewClock(start)
	s := newTestStore(t)
	s.Now, s.IDs = sclk.Now, testkit.NewUUIDv7s(sclk.Now)
	m := memstore.New()
	m.Now, m.IDs = mclk.Now, testkit.NewUUIDv7s(mclk.Now)

	both := func(cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, contracts.ApplyResult) {
		t.Helper()
		if head, _ := m.Head(ctx); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		want, err := m.Apply(ctx, proto.CloneOf(cs))
		if err != nil {
			t.Fatalf("%s: reference: %v", cs.GetEventId(), err)
		}
		got, err := s.Apply(ctx, cs)
		if err != nil {
			t.Fatalf("%s: %v", cs.GetEventId(), err)
		}
		return got, want
	}
	facts := func(subject, object string, ppm uint32) ([]*modelv1alpha1.SupportTimeline, []*modelv1alpha1.FactTimeline) {
		o := &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue(object)}
		return []*modelv1alpha1.SupportTimeline{{
			Source: "github-acme", SubjectId: subject, Predicate: "name", Object: o,
			Versions: []*modelv1alpha1.Support{{Source: "github-acme", ConfidencePpm: proto.Uint32(ppm), Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT, EventId: "x", ObservedAt: timestamppb.New(sclk.Now())}},
		}}, []*modelv1alpha1.FactTimeline{{
			SubjectId: subject, Predicate: "name", Object: o,
			Spans: []*modelv1alpha1.FactSpan{{Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: ppm}},
		}}
	}
	sup, fct := facts("new:l", "payments", 800_000)
	setup, _ := both(&modelv1alpha1.ChangeSet{
		EventId: "e1",
		Mints: []*modelv1alpha1.Mint{
			{Ref: "new:p", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION},
			{Ref: "new:l", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION},
		},
		Bindings: []*modelv1alpha1.BindingTimeline{
			{Alias: "github:team_node/T_p", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/T_p", SubjectId: "new:p"}}},
			{Alias: "github:team_node/T_l", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/T_l", SubjectId: "new:l"}}},
		},
		Supports: sup, Facts: fct,
	})
	p, l := string(setup.Subjects["new:p"]), string(setup.Subjects["new:l"])
	both(&modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})

	sup, fct = facts("new:back", "payments", 900_000)
	got, want := both(&modelv1alpha1.ChangeSet{
		EventId:  "e3",
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:back"}},
		Bindings: []*modelv1alpha1.BindingTimeline{{Alias: "github:team_node/T_l", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/T_l", SubjectId: "new:back"}}}},
		Supports: sup, Facts: fct,
	})
	if got.Subjects["new:back"] != want.Subjects["new:back"] {
		t.Fatalf("got %v, want %v", got.Subjects, want.Subjects)
	}
	f := contracts.FactFilter{SubjectID: contracts.SubjectID(l)}
	gotFacts, err := s.AsOf(ctx, f, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantFacts, _ := m.AsOf(ctx, f, time.Time{}, time.Time{})
	if len(wantFacts) != 1 || len(gotFacts) != len(wantFacts) || !proto.Equal(gotFacts[0], wantFacts[0]) {
		t.Fatalf("got %v, want %v", gotFacts, wantFacts)
	}
	if n := count(t, s, `SELECT count(*) FROM series WHERE tbl = $1`, int16(memstore.TableFacts)); n != 1 {
		t.Fatalf("got %d fact series, want the one that exists", n)
	}
}

// An operation loads the merge records of the components of the subjects it
// names (#81), not the whole merge table, however large that grows.
func TestOperationsLoadOnlyTheMergeComponentsTheyName(t *testing.T) {
	ctx := context.Background()
	s, _ := graphStoreAt(t)
	// Twenty components of three subjects each, in chains of two merges.
	var chains [][]string
	for i := range 20 {
		ids := mintTeams(t, s, fmt.Sprintf("mint-%d", i), 3)
		merge(t, s, fmt.Sprintf("m1-%d", i), ids[0], ids[1])
		merge(t, s, fmt.Sprintf("m2-%d", i), ids[0], ids[2])
		chains = append(chains, ids)
	}
	if n := count(t, s, `SELECT count(*) FROM merge_record`); n != 40 {
		t.Fatalf("got %d merge records, want 40", n)
	}
	loadedMerges := func(sc scope) int {
		t.Helper()
		var ld *loaded
		err := s.readTx(ctx, func(q querier) error {
			meta, err := readMeta(ctx, q, false)
			if err != nil {
				return err
			}
			ld, err = load(ctx, q, meta, sc)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return len(ld.mergeRecords)
	}
	// Naming the merged-away end of one chain loads that chain's two
	// records and nothing else.
	if got := loadedMerges(scope{Subjects: []string{chains[7][2]}, Rows: true}); got != 2 {
		t.Errorf("one component: loaded %d merge records, want 2", got)
	}
	if got := loadedMerges(scope{Subjects: []string{chains[3][1], chains[9][0]}, Rows: true}); got != 4 {
		t.Errorf("two components: loaded %d merge records, want 4", got)
	}
	if got := loadedMerges(scope{Subjects: []string{"not-a-subject"}}); got != 0 {
		t.Errorf("an unknown subject: loaded %d merge records, want 0", got)
	}
	// A subject nobody merged is in no component.
	lone := mintTeams(t, s, "lone", 1)[0]
	if got := loadedMerges(scope{Subjects: []string{lone}}); got != 0 {
		t.Errorf("an unmerged subject: loaded %d merge records, want 0", got)
	}
	// Joining two components joins their records.
	merge(t, s, "join", chains[1][0], chains[2][0])
	if got := loadedMerges(scope{Subjects: []string{chains[2][2]}}); got != 5 {
		t.Errorf("joined components: loaded %d merge records, want 5", got)
	}
	// And the answers are right.
	sub, err := s.Subject(ctx, contracts.SubjectID(chains[2][2]), time.Time{})
	if err != nil || sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED || sub.GetMergedInto() != chains[2][0] {
		t.Fatalf("got %v, %v, want merged into %s", sub, err, chains[2][0])
	}
}

// Apply refuses a ChangeSet while a restore is running or did not finish, and
// so does every read.
func TestAStoreMidRestoreRefusesEverythingButRestore(t *testing.T) {
	ctx := context.Background()
	src, clk := graphStoreAt(t)
	for i := range 3 {
		cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("e%d", i), State: stateEntries(2, fmt.Sprintf("s%d", i))}
		if head, _ := src.Head(ctx); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		if _, err := src.Apply(ctx, cs); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}
	var backup bytes.Buffer
	if err := src.Backup(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	want, _ := src.Head(ctx)

	// A restore that died after two entries: it left the marker and the
	// partial graph, and took its lock with it.
	dst, o := openTestStore(t)
	dst.Now, dst.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	if err := dst.beginRestore(ctx); err != nil {
		t.Fatal(err)
	}
	br, err := contracts.NewBackupReader(bytes.NewReader(backup.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		rec, err := br.Next()
		if err != nil {
			t.Fatal(err)
		}
		e := &modelv1alpha1.JournalEntry{}
		if err := proto.Unmarshal(rec, e); err != nil {
			t.Fatal(err)
		}
		if err := dst.replay(ctx, e, br.Header.GetTakenAt().AsTime()); err != nil {
			t.Fatal(err)
		}
	}
	other := reopen(t, o, clk) // a restarted process
	for name, err := range map[string]error{
		"Head":   func() error { _, err := other.Head(ctx); return err }(),
		"State":  func() error { _, err := other.State(ctx, []string{"s0-00000"}, time.Time{}); return err }(),
		"Apply":  func() error { _, err := other.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "late"}); return err }(),
		"Backup": other.Backup(ctx, &bytes.Buffer{}),
	} {
		if !errors.Is(err, ErrRestoring) {
			t.Errorf("%s: got %v, want ErrRestoring", name, err)
		}
	}
	// Running Restore again starts over and finishes.
	if err := other.Restore(ctx, bytes.NewReader(backup.Bytes())); err != nil {
		t.Fatal(err)
	}
	if got, err := other.Head(ctx); err != nil || !got.Equal(want) {
		t.Fatalf("got head %v, %v, want %v", got, err, want)
	}
	if n := count(t, other, `SELECT count(*) FROM journal`); n != 3 {
		t.Fatalf("got %d journal entries after the second restore, want 3", n)
	}
	// Not into a store that holds a graph.
	if err := other.Restore(ctx, bytes.NewReader(backup.Bytes())); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("got %v, want a refusal to restore into a store that holds a graph", err)
	}
}

// Two restores into one store don't clear each other's work.
func TestOnlyOneRestoreRunsAtATime(t *testing.T) {
	ctx := context.Background()
	s, _ := graphStoreAt(t)
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "e", State: stateEntries(1, "s")}); err != nil {
		t.Fatal(err)
	}
	var backup bytes.Buffer
	if err := s.Backup(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	dst := newTestStore(t)
	// Hold the lock as a running restore does.
	tx, err := dst.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || current_schema(), 0))`, restoreLock); err != nil {
		t.Fatal(err)
	}
	if err := dst.Restore(ctx, bytes.NewReader(backup.Bytes())); err == nil || !strings.Contains(err.Error(), "another restore is running") {
		t.Fatalf("got %v, want a refusal while another restore holds the lock", err)
	}
}

// Backup fails when the journal has a gap rather than writing a short
// backup with a valid trailer (#96).
func TestBackupFailsOnAGapInTheJournal(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	for i := range 4 {
		cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("e%d", i), State: stateEntries(1, fmt.Sprintf("s%d", i))}
		if head, _ := s.Head(ctx); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		if _, err := s.Apply(ctx, cs); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
	}
	if err := s.Backup(ctx, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `DELETE FROM journal WHERE seq = 2`)
	if err := s.Backup(ctx, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "entry 3 where entry 2 should be") {
		t.Fatalf("got %v, want a gap in the journal", err)
	}
	mustExec(t, s, `DELETE FROM journal WHERE seq = 4`)
	if err := s.Backup(ctx, &bytes.Buffer{}); err == nil {
		t.Fatal("backed up a journal that ends short of the head")
	}
}

func mustExec(t testing.TB, s *Store, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
