package surrealstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

// The conformance suites need a SurrealDB to talk to. Set
// BEARING_TEST_SURREALDB to a server URL (for example ws://127.0.0.1:8000,
// started with `surreal start --user root --pass root memory`) and
// BEARING_TEST_SURREALDB_USER and BEARING_TEST_SURREALDB_PASS to sign in, or
// build with -tags surrealembed to use an embedded engine. Otherwise the
// tests skip.
var dbSeq atomic.Int64

// scopedTests says to run the suites as a database-scoped user, as Bearing
// does in a deployment (C-STORE-2). BEARING_TEST_SURREALDB_USER and _PASS
// are then the root credentials that provision it. CI sets
// BEARING_TEST_SURREALDB_SCOPED and starts the server with network access
// and scripting denied (C-STORE-4).
func scopedTests() bool { return os.Getenv("BEARING_TEST_SURREALDB_SCOPED") != "" }

// provisionScoped creates o's database and a database user for it and
// returns o set to sign in as that user.
func provisionScoped(t *testing.T, o ServerOptions) ServerOptions {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	user, pass := "bearing", randomToken()
	err := Provision(ctx, ProvisionOptions{
		URL: o.URL, Namespace: o.Namespace, Database: o.Database,
		AdminUsername: o.Username, AdminPassword: o.Password, Username: user, Password: pass,
	})
	if err != nil {
		t.Fatal(err)
	}
	o.Username, o.Password, o.Scoped = user, pass, true
	return o
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A fresh database per test keeps them independent.
	db := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), dbSeq.Add(1))
	var (
		s   *Store
		err error
	)
	switch url := os.Getenv("BEARING_TEST_SURREALDB"); {
	case url != "":
		o := ServerOptions{
			URL: url, Namespace: "bearing_test", Database: db,
			Username: os.Getenv("BEARING_TEST_SURREALDB_USER"), Password: os.Getenv("BEARING_TEST_SURREALDB_PASS"),
		}
		if scopedTests() {
			o = provisionScoped(t, o)
		}
		s, err = Dial(ctx, o)
	case EmbeddedAvailable:
		s, err = OpenEmbedded(ctx, "mem://", "bearing_test", db)
	default:
		t.Skip("set BEARING_TEST_SURREALDB or build with -tags surrealembed")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func TestDialRejectsBadCredentials(t *testing.T) {
	url, user := os.Getenv("BEARING_TEST_SURREALDB"), os.Getenv("BEARING_TEST_SURREALDB_USER")
	if url == "" || user == "" {
		t.Skip("set BEARING_TEST_SURREALDB and BEARING_TEST_SURREALDB_USER")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Dial(ctx, ServerOptions{
		URL: url, Namespace: "bearing_test", Database: "bad_credentials",
		Username: user, Password: os.Getenv("BEARING_TEST_SURREALDB_PASS") + "-wrong",
	})
	if err == nil {
		_ = s.Close(ctx)
		t.Fatal("signed in with the wrong password")
	}
	if !strings.Contains(err.Error(), "surrealstore: sign in: ") || strings.Contains(err.Error(), user) {
		t.Fatalf("got %v, want a sign-in error that does not name the user", err)
	}
}

// A failed transaction reports why it failed, not that the other statements
// did not run.
func TestQueryReportsTheStatementThatFailed(t *testing.T) {
	s := newTestStore(t)
	_, err := s.q.Query(context.Background(), `
BEGIN TRANSACTION;
CREATE probe:1;
THROW "probe failed";
COMMIT TRANSACTION;`, nil)
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("got %v, want the thrown error", err)
	}
}

func TestStatementErrorPicksTheCause(t *testing.T) {
	const skipped = "The query was not executed due to a failed transaction"
	for _, tc := range []struct {
		name string
		msgs []string
		want string
	}{
		{"cause after skipped statements", []string{skipped, "An error occurred: boom", skipped}, "An error occurred: boom"},
		{"only skipped statements", []string{skipped, "The query was not executed due to a cancelled transaction"}, skipped},
		{"no messages", nil, "query failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statementError(tc.msgs).Error(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGraphConformance(t *testing.T) {
	conformance.GraphStore(t, func(t *testing.T) (contracts.GraphStore, conformance.Clock, conformance.IDs) {
		clk := testkit.NewClock(time.Time{})
		s := newTestStore(t)
		ids := testkit.NewUUIDv7s(clk.Now)
		s.Now, s.IDs = clk.Now, ids
		return s, clk, ids
	})
}

func TestVectorConformance(t *testing.T) {
	conformance.VectorIndex(t, func(t *testing.T) contracts.VectorIndex { return newTestStore(t) })
}

func TestReopenKeepsVectorDimension(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Upsert(ctx, []contracts.VectorPoint{{ID: "a", SubjectID: "a", Vector: []float32{1, 0, 0}}}); err != nil {
		t.Fatal(err)
	}
	again, err := New(ctx, s.q)
	if err != nil {
		t.Fatal(err)
	}
	if again.dim != 3 {
		t.Fatalf("dim = %d, want 3", again.dim)
	}
	if err := again.Upsert(ctx, []contracts.VectorPoint{{ID: "b", SubjectID: "a", Vector: []float32{1, 0}}}); err == nil {
		t.Fatal("expected a dimension mismatch error")
	}
}

// An operator may put a secret in the server URL's path or query by mistake;
// the connect error names only the scheme and host (C-SECRET-2).
func TestDialConnectErrorOmitsPathAndQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, tc := range []struct{ url, wantHost string }{
		{"ws://127.0.0.1:1/s3cr3t", "127.0.0.1:1"},
		{"ws://127.0.0.1:1/?token=s3cr3t", "127.0.0.1:1"},
		{"http://127.0.0.1:1/s3cr3t", "127.0.0.1:1"},
		// The driver quotes a URL it can't parse; Dial refuses these first.
		{"ws://[::1/s3cr3t", ""},
		{"://s3cr3t", ""},
		{"ws://host\x7f/s3cr3t", ""},
		{"ws://127.0.0.1:1#s3cr3t", ""},
	} {
		t.Run(tc.url, func(t *testing.T) {
			s, err := Dial(ctx, ServerOptions{URL: tc.url, Namespace: "n", Database: "d"})
			if err == nil {
				_ = s.Close(ctx)
				t.Fatal("connected to a closed port")
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("error leaks the URL's path or query: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantHost) {
				t.Fatalf("got %v, want the error to name %q", err, tc.wantHost)
			}
		})
	}
}

func TestSafeNameKeepsSchemeAndHost(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"ws://db.internal:8000/rpc?x=1", "ws://db.internal:8000"},
		{"https://db.internal", "https://db.internal"},
		{"not a url", "the server"},
		{"%zz", "the server"},
	} {
		if got := safeName(tc.raw); got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// graphStore opens a second store on s's database, as another process would,
// with its own clock and ID source.
func graphStore(t *testing.T, s *Store, clk *testkit.FakeClock) *Store {
	t.Helper()
	again, err := New(context.Background(), s.q)
	if err != nil {
		t.Fatal(err)
	}
	again.Now, again.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	return again
}

func graphStoreAt(t *testing.T) (*Store, *testkit.FakeClock) {
	t.Helper()
	clk := testkit.NewClock(time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC))
	s := newTestStore(t)
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	return s, clk
}

func stateEntries(n int, prefix string) []*modelv1alpha1.StateEntry {
	out := make([]*modelv1alpha1.StateEntry, n)
	for i := range out {
		v, _ := anypb.New(wrapperspb.Int64(int64(i)))
		out[i] = &modelv1alpha1.StateEntry{Key: fmt.Sprintf("%s-%05d", prefix, i), Value: v}
	}
	return out
}

func count(t *testing.T, s *Store, query string, vars map[string]any) int {
	t.Helper()
	res, err := s.q.Query(context.Background(), query, vars)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		N int `json:"n"`
	}
	if err := decode(res[0], &rows); err != nil {
		t.Fatalf("decode %v: %v", res[0], err)
	}
	if len(rows) == 0 {
		return 0 // GROUP ALL over no rows
	}
	return rows[0].N
}

// A second store on the same database reads what the first applied, with
// the refs of a ChangeSet resolved in its facts, and answers a repeated
// event with the original result.
func TestGraphSurvivesReopeningTheDatabase(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
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
			Spans: []*modelv1alpha1.FactSpan{{
				Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: 1_000_000,
			}},
		}},
		State: stateEntries(1, "cursor"),
	}
	res, err := s.Apply(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	id := string(res.Subjects["new:r"])

	other := graphStore(t, s, clk)
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
	s := newTestStore(t)
	res, err := s.q.Query(ctx, `SELECT VALUE version FROM schema_version ORDER BY version`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	if err := decode(res[0], &got); err != nil {
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
	if _, err := New(ctx, s.q); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count() AS n FROM schema_version GROUP ALL`, nil); n != len(migrations) {
		t.Fatalf("got %d versions after reopening, want %d", n, len(migrations))
	}
	if _, err := s.q.Query(ctx, `CREATE schema_version:99 CONTENT { version: 99, name: 'from the future' }`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, s.q); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("got %v, want a refusal naming version 99", err)
	}
}

// Rows an apply wrote ahead of its commit are invisible, and the next
// commit deletes the ones that never committed.
func TestRowsOfAnUncommittedApplyAreInvisibleAndCleared(t *testing.T) {
	ctx := context.Background()
	s, _ := graphStoreAt(t)
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "first", State: stateEntries(1, "kept")}); err != nil {
		t.Fatal(err)
	}
	head, err := s.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// What a lost attempt leaves behind: a state series with a row, and a
	// subject, recorded after the head.
	lost := microsOf(head) + 10
	msg := func(m proto.Message) []byte {
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	entry := stateEntries(1, "lost")[0]
	if _, err := s.q.Query(ctx, `
INSERT INTO series { tbl: 3, key: 'lost-00000', predicate: '', head: $head, rec: $rec, stg: 'lost' };
INSERT INTO version { tbl: 3, key: 'lost-00000', n: 0, rec: $rec, ret: 0, data: $data, stg: 'lost' };
INSERT INTO subject { sid: 'lost-subject', kind: 'Team', rec: $rec, stg: 'lost', data: $head };`,
		map[string]any{"rec": lost, "head": msg(&modelv1alpha1.StateEntry{Key: "lost-00000"}), "data": msg(entry)}); err != nil {
		t.Fatal(err)
	}
	st, err := s.State(ctx, []string{"lost-00000", "kept-00000"}, time.Time{})
	if err != nil || len(st) != 1 || st["kept-00000"] == nil {
		t.Fatalf("got %v, %v, want only the committed entry", st, err)
	}
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "second", BaseRecordedAt: timestamppb.New(head)}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"series", "version", "subject"} {
		if n := count(t, s, `SELECT count() AS n FROM type::table($t) WHERE stg = 'lost' GROUP ALL`, map[string]any{"t": table}); n != 0 {
			t.Errorf("%d rows of the lost attempt remain in %s", n, table)
		}
	}
}

// An apply too large for one transaction stages its rows first. If another
// apply commits before it does, the staged rows are deleted by that commit,
// never shown, and the large apply reports the head moved.
func TestLargeApplyStagesRowsAndLosesCleanly(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	other := graphStore(t, s, clk)
	big := &modelv1alpha1.ChangeSet{EventId: "big", State: stateEntries(inlineRows, "big")} // two rows each
	moved := false
	s.afterStage = func() {
		if moved {
			return
		}
		moved = true
		if _, err := other.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "small", State: stateEntries(1, "small")}); err != nil {
			t.Error(err)
		}
	}
	_, err := s.Apply(ctx, big)
	if !errors.Is(err, contracts.ErrStale) {
		// The ChangeSet read an empty store; the head has moved on.
		t.Fatalf("got %v, want ErrStale", err)
	}
	if !moved {
		t.Fatal("the large apply did not stage its rows")
	}
	if n := count(t, s, `SELECT count() AS n FROM version WHERE stg != NONE AND key CONTAINS 'big' GROUP ALL`, nil); n != 0 {
		t.Fatalf("%d staged rows of the lost apply remain", n)
	}
	st, err := s.State(ctx, []string{"big-00000", "small-00000"}, time.Time{})
	if err != nil || len(st) != 1 || st["small-00000"] == nil {
		t.Fatalf("got %v, %v, want only the committed apply", st, err)
	}
	// Applied against the new head, it commits.
	head, _ := s.Head(ctx)
	big.BaseRecordedAt = timestamppb.New(head)
	if _, err := s.Apply(ctx, big); err != nil {
		t.Fatal(err)
	}
	if st, err := s.State(ctx, []string{"big-00000", fmt.Sprintf("big-%05d", inlineRows-1)}, time.Time{}); err != nil || len(st) != 2 {
		t.Fatalf("got %v, %v, want both ends of the large apply", st, err)
	}
}

func TestMergeComponentsExpandBothWays(t *testing.T) {
	merges := []*modelv1alpha1.MergeRecord{
		{SurvivorId: "a", MergedId: "b"},
		{SurvivorId: "b", MergedId: "c"},
		{SurvivorId: "x", MergedId: "y"},
	}
	c := components(merges)
	for _, tc := range []struct {
		in, want []string
	}{
		{[]string{"c"}, []string{"a", "b", "c"}},
		{[]string{"a"}, []string{"a", "b", "c"}},
		{[]string{"y", "z"}, []string{"x", "y", "z"}},
		{[]string{"", "q"}, []string{"q"}},
		{nil, []string{}},
	} {
		if got := c.expand(tc.in); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("expand(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// In a scoped run the store's user is confined to its database and the
// server refuses network access and scripting from queries (C-STORE-2,
// C-STORE-4).
func TestScopedUserIsConfinedToItsDatabase(t *testing.T) {
	if !scopedTests() || os.Getenv("BEARING_TEST_SURREALDB") == "" {
		t.Skip("set BEARING_TEST_SURREALDB and BEARING_TEST_SURREALDB_SCOPED")
	}
	ctx := context.Background()
	s := newTestStore(t)
	for name, sql := range map[string]string{
		"define another namespace": `DEFINE NAMESPACE elsewhere`,
		"define another database":  `DEFINE DATABASE elsewhere`,
		"define a user":            `DEFINE USER intruder ON DATABASE PASSWORD 'x' ROLES OWNER`,
		"read another database":    `USE NS bearing_test DB another; SELECT * FROM schema_version`,
		"read another namespace":   `USE NS another DB another; SELECT * FROM schema_version`,
		// The server's own health endpoint: reachable, so only the
		// capability can refuse it.
		"outbound http": `RETURN http::get('http://` + strings.TrimPrefix(strings.TrimPrefix(os.Getenv("BEARING_TEST_SURREALDB"), "wss://"), "ws://") + `/health')`,
		"scripting":     `RETURN function() { return 1; }`,
	} {
		_, err := s.q.Query(ctx, sql, nil)
		if err == nil {
			t.Errorf("%s: the store's user was allowed to", name)
		}
	}
	// What it needs still works: define tables and use them.
	if _, err := s.q.Query(ctx, `DEFINE TABLE IF NOT EXISTS probe SCHEMALESS; CREATE probe:1 SET n = 1`, nil); err != nil {
		t.Errorf("define and write a table: %v", err)
	}
}
