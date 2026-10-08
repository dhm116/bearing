package surrealstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
)

var errInjected = errors.New("injected failure")

// writes matches the statements that change the database.
var writes = regexp.MustCompile(`(?i)\b(INSERT|CREATE|UPDATE|UPSERT|DELETE|DEFINE|THROW)\b`)

// faultQ runs its queries against q until the failAt-th, which fails before
// reaching the database (failAt 0 never fails). With garble set, the results
// of queries that only read come back as strings that decode into nothing.
type faultQ struct {
	Querier
	failAt, calls int
	garble        bool
}

func (f *faultQ) Query(ctx context.Context, sql string, vars map[string]any) ([]any, error) {
	f.calls++
	if f.failAt != 0 && f.calls == f.failAt {
		if !f.garble {
			return nil, errInjected
		}
		if !writes.MatchString(sql) {
			res, err := f.Querier.Query(ctx, sql, vars)
			if err != nil {
				return nil, err
			}
			for i := range res {
				res[i] = "not a result"
			}
			return res, nil
		}
	}
	return f.Querier.Query(ctx, sql, vars)
}

// withQ returns a store like s that talks through q. It shares s's clock and
// IDs, so an operation through it continues the same history.
func withQ(s *Store, q Querier) *Store {
	return &Store{q: q, Now: s.Now, IDs: s.IDs, NewID: s.NewID, afterStage: s.afterStage, dim: s.dim, maxMerges: s.maxMerges}
}

// Whatever query fails, or comes back unreadable, an operation reports an
// error rather than a wrong answer, and the store works afterwards.
func TestEveryQueryMayFailWithoutHarmingTheStore(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	object := &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue("payments")}
	base := &modelv1alpha1.ChangeSet{
		EventId: "base",
		Mints:   []*modelv1alpha1.Mint{{Ref: "new:r", Kind: "Repository", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}},
		Bindings: []*modelv1alpha1.BindingTimeline{{Alias: "github:repo_node/R_1", Bindings: []*modelv1alpha1.Binding{
			{Alias: "github:repo_node/R_1", SubjectId: "new:r"},
		}}},
		Supports: []*modelv1alpha1.SupportTimeline{{
			Source: "github-acme", SubjectId: "new:r", Predicate: "name", Object: object,
			Versions: []*modelv1alpha1.Support{{Source: "github-acme", ConfidencePpm: proto.Uint32(1_000_000), Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT, EventId: "base", ObservedAt: timestamppb.New(clk.Now())}},
		}},
		Facts: []*modelv1alpha1.FactTimeline{{
			SubjectId: "new:r", Predicate: "name", Object: object,
			Spans: []*modelv1alpha1.FactSpan{{Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: 1_000_000}},
		}},
		State: stateEntries(2, "cursor"),
	}
	res, err := s.Apply(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	id := res.Subjects["new:r"]
	key := "github:repo_node/R_1"

	// next is a ChangeSet on a new event, based on the head at that moment.
	n := 0
	// Its state keys are new, so every attempt writes the same rows.
	next := func(rows int) *modelv1alpha1.ChangeSet {
		n++
		head, _ := s.Head(ctx)
		return &modelv1alpha1.ChangeSet{
			EventId: fmt.Sprintf("fault-%d", n), BaseRecordedAt: timestamppb.New(head),
			State: stateEntries(rows, fmt.Sprintf("k%d", n)),
		}
	}
	ops := []struct {
		name string
		do   func(ctx context.Context, s *Store) error
	}{
		{"Apply", func(ctx context.Context, s *Store) error {
			_, err := s.Apply(ctx, next(2))
			return err
		}},
		{"Apply of a duplicate", func(ctx context.Context, s *Store) error {
			_, err := s.Apply(ctx, base)
			return err
		}},
		{"Head", func(ctx context.Context, s *Store) error { _, err := s.Head(ctx); return err }},
		{"Subject", func(ctx context.Context, s *Store) error { _, err := s.Subject(ctx, id, time.Time{}); return err }},
		{"ResolveKey", func(ctx context.Context, s *Store) error {
			_, err := s.ResolveKey(ctx, model.Key(key), time.Time{}, time.Time{})
			return err
		}},
		{"Bindings", func(ctx context.Context, s *Store) error {
			_, err := s.Bindings(ctx, []model.Key{model.Key(key)}, []contracts.SubjectID{id}, time.Time{})
			return err
		}},
		{"Merges", func(ctx context.Context, s *Store) error { _, err := s.Merges(ctx, id, time.Time{}); return err }},
		{"State", func(ctx context.Context, s *Store) error {
			_, err := s.State(ctx, []string{"cursor-00000"}, time.Time{})
			return err
		}},
		{"Supports", func(ctx context.Context, s *Store) error {
			_, err := s.Supports(ctx, contracts.SupportFilter{SubjectID: id, Predicate: "name"}, time.Time{})
			return err
		}},
		{"AsOf by subject", func(ctx context.Context, s *Store) error {
			_, err := s.AsOf(ctx, contracts.FactFilter{SubjectID: id}, time.Time{}, time.Time{})
			return err
		}},
		{"AsOf by key", func(ctx context.Context, s *Store) error {
			_, err := s.AsOf(ctx, contracts.FactFilter{Key: model.Key(key)}, time.Time{}, time.Time{})
			return err
		}},
		{"Changes", func(ctx context.Context, s *Store) error {
			_, err := s.Changes(ctx, contracts.FactFilter{SubjectID: id}, time.Time{}, clk.Now().Add(time.Hour), contracts.AxisRecord)
			return err
		}},
		{"Conflicts", func(ctx context.Context, s *Store) error {
			_, err := s.Conflicts(ctx, id, "name", time.Time{}, time.Time{})
			return err
		}},
		{"DataQuality", func(ctx context.Context, s *Store) error {
			_, err := s.DataQuality(ctx, contracts.IssueFilter{Kinds: []string{"Repository"}}, time.Time{}, time.Time{})
			return err
		}},
		{"Backup", func(ctx context.Context, s *Store) error { return s.Backup(ctx, io.Discard) }},
		{"Apply staging its rows", func(ctx context.Context, s *Store) error {
			_, err := s.Apply(ctx, next(inlineRows))
			return err
		}},
	}
	for _, op := range ops {
		for _, garble := range []bool{false, true} {
			// The staging apply is slow, and a garbled run adds nothing to it.
			if garble && op.name == "Apply staging its rows" {
				continue
			}
			t.Run(fmt.Sprintf("%s garble=%v", op.name, garble), func(t *testing.T) {
				counter := &faultQ{Querier: s.q}
				if err := op.do(ctx, withQ(s, counter)); err != nil {
					t.Fatal(err)
				}
				for at := 1; at <= counter.calls; at++ {
					f := &faultQ{Querier: s.q, failAt: at, garble: garble}
					err := op.do(ctx, withQ(s, f))
					if !garble && !errors.Is(err, errInjected) {
						t.Fatalf("query %d of %d: got %v, want the injected failure", at, counter.calls, err)
					}
					// An unreadable result may still be ignored by a statement
					// whose result nothing reads; the store must be fine either way.
					if _, err := s.Head(ctx); err != nil {
						t.Fatalf("query %d: store unusable afterwards: %v", at, err)
					}
					if err := op.do(ctx, s); err != nil {
						t.Fatalf("query %d: retry after failure: %v", at, err)
					}
				}
			})
		}
	}
}

// Restore loads a backup into an empty store, and a failed Restore leaves
// the store empty so that another can follow.
func TestRestoreFailingPartwayLeavesTheStoreEmpty(t *testing.T) {
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
	var buf bytes.Buffer
	if err := src.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	backup := buf.Bytes()

	dst, _ := graphStoreAt(t)
	counter := &faultQ{Querier: dst.q}
	if err := withQ(dst, counter).Restore(ctx, bytes.NewReader(backup)); err != nil {
		t.Fatal(err)
	}
	total := counter.calls
	want, _ := src.Head(ctx)
	if got, _ := dst.Head(ctx); !got.Equal(want) {
		t.Fatalf("got head %v, want %v", got, want)
	}

	for at := 1; at <= total; at++ {
		fresh, _ := graphStoreAt(t)
		f := &faultQ{Querier: fresh.q, failAt: at}
		if err := withQ(fresh, f).Restore(ctx, bytes.NewReader(backup)); err == nil {
			t.Fatalf("query %d of %d: Restore succeeded despite the failure", at, total)
		}
		if err := fresh.Restore(ctx, bytes.NewReader(backup)); err != nil {
			t.Fatalf("query %d of %d: Restore after the failure: %v", at, total, err)
		}
		if got, _ := fresh.Head(ctx); !got.Equal(want) {
			t.Fatalf("query %d: got head %v, want %v", at, got, want)
		}
	}
}

// Opening a store survives a failure at any step of the migrations.
func TestNewFailingAtAnyQueryCanBeRetried(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	fresh := &faultQ{Querier: s.q}
	if _, err := New(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	for at := 1; at <= fresh.calls; at++ {
		f := &faultQ{Querier: s.q, failAt: at}
		if _, err := New(ctx, f); !errors.Is(err, errInjected) {
			t.Fatalf("query %d of %d: got %v, want the injected failure", at, fresh.calls, err)
		}
		if _, err := New(ctx, s.q); err != nil {
			t.Fatalf("query %d: open after the failure: %v", at, err)
		}
	}
}

// fakeQ answers every query with an error, or nothing if err is nil.
type fakeQ struct{ err error }

func (f fakeQ) Query(context.Context, string, map[string]any) ([]any, error) { return nil, f.err }
func (f fakeQ) Close(context.Context) error                                  { return nil }

func TestSelectingADatabaseNeedsNamesAndReportsFailures(t *testing.T) {
	ctx := context.Background()
	use := func(err error) func(context.Context, string, string) error {
		return func(context.Context, string, string) error { return err }
	}
	for _, sel := range []struct {
		name string
		do   func(q Querier, ns, db string, use func(context.Context, string, string) error) error
	}{
		{"selectDatabase", func(q Querier, ns, db string, u func(context.Context, string, string) error) error {
			return selectDatabase(ctx, q, ns, db, u)
		}},
		{"useDatabase", func(q Querier, ns, db string, u func(context.Context, string, string) error) error {
			return useDatabase(ctx, q, ns, db, u)
		}},
	} {
		t.Run(sel.name, func(t *testing.T) {
			if err := sel.do(fakeQ{}, "", "db", use(nil)); err == nil {
				t.Error("accepted an empty namespace")
			}
			if err := sel.do(fakeQ{}, "ns", "", use(nil)); err == nil {
				t.Error("accepted an empty database")
			}
			if err := sel.do(fakeQ{}, "ns", "db", use(errInjected)); !errors.Is(err, errInjected) {
				t.Errorf("got %v, want the failure of use", err)
			}
			if err := sel.do(fakeQ{}, "ns", "db", use(nil)); err != nil {
				t.Errorf("got %v, want success", err)
			}
		})
	}
	// useDatabase also fails if the database cannot be defined, or cannot be
	// selected once it is.
	if err := useDatabase(ctx, fakeQ{err: errInjected}, "ns", "db", use(nil)); !errors.Is(err, errInjected) {
		t.Errorf("got %v, want the failure to define", err)
	}
	calls := 0
	second := func(context.Context, string, string) error {
		if calls++; calls == 2 {
			return errInjected
		}
		return nil
	}
	if err := useDatabase(ctx, fakeQ{}, "ns", "db", second); !errors.Is(err, errInjected) {
		t.Errorf("got %v, want the failure to select after defining", err)
	}
}

func TestProvisionRefusesWhatItCannotDo(t *testing.T) {
	url := os.Getenv("BEARING_TEST_SURREALDB")
	if url == "" {
		t.Skip("set BEARING_TEST_SURREALDB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin := ProvisionOptions{
		URL: url, Namespace: "bearing_test", Database: "provision_refusals",
		AdminUsername: os.Getenv("BEARING_TEST_SURREALDB_USER"), AdminPassword: os.Getenv("BEARING_TEST_SURREALDB_PASS"),
		Username: "bearing", Password: "a-password",
	}
	cases := map[string]func(o *ProvisionOptions){
		"no user":      func(o *ProvisionOptions) { o.Username = "" },
		"no password":  func(o *ProvisionOptions) { o.Password = "" },
		"unreachable":  func(o *ProvisionOptions) { o.URL = "ws://127.0.0.1:1/secret?token=abc" },
		"wrong admin":  func(o *ProvisionOptions) { o.AdminPassword += "-wrong" },
		"no namespace": func(o *ProvisionOptions) { o.Namespace = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			o := admin
			change(&o)
			err := Provision(ctx, o)
			if err == nil {
				t.Fatal("provisioned despite the problem")
			}
			for _, secret := range []string{"a-password", "abc", "/secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q leaks %q", err, secret)
				}
			}
		})
	}
}

// Names go into DEFINE statements, which take no parameters, so they are
// checked, not escaped.
func TestIdentAcceptsOnlyPlainNames(t *testing.T) {
	for _, ok := range []string{"bearing", "main", "t1759_3", "my-db.v2", strings.Repeat("a", 64)} {
		if got, err := ident(ok); err != nil || got != "`"+ok+"`" {
			t.Errorf("ident(%q) = %q, %v, want it quoted", ok, got, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 65), "a b", "x`", "x\\`; DEFINE NAMESPACE pwned; -- ", "a;b", "é", "a\nb", "a'b", `a"b`} {
		got, err := ident(bad)
		if err == nil || got != "" {
			t.Errorf("ident(%q) = %q, %v, want an error", bad, got, err)
		}
		if err != nil && strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("error %q repeats the name", err)
		}
	}
}

func FuzzIdentNeverQuotesAnythingButAName(f *testing.F) {
	for _, seed := range []string{"bearing", "x\\`; DEFINE NAMESPACE pwned; -- ", "a`b", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ident(s)
		if err != nil {
			return
		}
		body := strings.TrimSuffix(strings.TrimPrefix(got, "`"), "`")
		if body != s || strings.ContainsAny(body, "`\\;'\" \n") {
			t.Fatalf("ident(%q) = %q", s, got)
		}
	})
}

func TestProvisionRefusesNamesThatAreNotNames(t *testing.T) {
	payload := "x\\`; DEFINE NAMESPACE pwned; -- "
	for name, change := range map[string]func(o *ProvisionOptions){
		"user":      func(o *ProvisionOptions) { o.Username = payload },
		"namespace": func(o *ProvisionOptions) { o.Namespace = payload },
		"database":  func(o *ProvisionOptions) { o.Database = payload },
	} {
		t.Run(name, func(t *testing.T) {
			// The server is never reached: the names are checked first.
			o := ProvisionOptions{URL: "ws://127.0.0.1:1", Namespace: "ns", Database: "db", AdminUsername: "root", AdminPassword: "root", Username: "bearing", Password: "pw"}
			change(&o)
			err := Provision(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), "names use only") {
				t.Fatalf("got %v, want the name refused", err)
			}
		})
	}
}

func TestDialScopedNeedsAUser(t *testing.T) {
	_, err := Dial(context.Background(), ServerOptions{URL: "ws://127.0.0.1:1", Namespace: "ns", Database: "db", Scoped: true})
	if err == nil || !strings.Contains(err.Error(), "needs a user") {
		t.Fatalf("got %v, want a refusal to connect without a user", err)
	}
}
