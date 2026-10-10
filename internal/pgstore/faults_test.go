package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

var errInjected = errors.New("injected failure")

// faultPool runs the database's statements until the failAt-th, which fails
// before reaching the database (failAt 0 never fails), and counts them. The
// failure is err, or errInjected. before, if set, runs ahead of each
// statement with its text and the transaction.
type faultPool struct {
	pool
	failAt, calls int
	lostAck       bool // the failAt-th statement, a Commit, lands and then reports err
	err           error
	before        func(ctx context.Context, sql string, tx pgx.Tx)
}

func (p *faultPool) fail() error {
	p.calls++
	if p.failAt != 0 && p.calls == p.failAt {
		if p.err != nil {
			return p.err
		}
		return errInjected
	}
	return nil
}

func (p *faultPool) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	if err := p.fail(); err != nil {
		return nil, err
	}
	tx, err := p.pool.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, p: p}, nil
}

type faultTx struct {
	pgx.Tx
	p *faultPool
}

func (t *faultTx) step(ctx context.Context, sql string) error {
	if t.p.before != nil {
		t.p.before(ctx, sql, t.Tx)
	}
	return t.p.fail()
}

func (t *faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := t.step(ctx, sql); err != nil {
		return pgconn.CommandTag{}, err
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *faultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := t.step(ctx, sql); err != nil {
		return nil, err
	}
	return t.Tx.Query(ctx, sql, args...)
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

func (t *faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := t.step(ctx, sql); err != nil {
		return failedRow{err}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t *faultTx) Commit(ctx context.Context) error {
	if t.p.lostAck && t.p.failAt != 0 && t.p.calls+1 == t.p.failAt {
		// The commit lands and the caller is told it failed.
		t.p.calls++
		if err := t.Tx.Commit(ctx); err != nil {
			return err
		}
		return t.p.err
	}
	if err := t.p.fail(); err != nil {
		_ = t.Rollback(ctx)
		return err
	}
	return t.Tx.Commit(ctx)
}

// withPool returns a store like s that talks through p. It shares s's clock
// and IDs, so an operation through it continues the same history.
func withPool(s *Store, p *faultPool) *Store {
	p.pool = s.db
	return &Store{db: p, Now: s.Now, IDs: s.IDs, vec: s.vec}
}

// Whatever statement fails, an operation reports an error rather than a
// wrong answer, writes nothing, and the store works afterwards.
func TestEveryStatementMayFailWithoutHarmingTheStore(t *testing.T) {
	t.Parallel()
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
	n := 0
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
		{"Apply", func(ctx context.Context, s *Store) error { _, err := s.Apply(ctx, next(2)); return err }},
		{"Apply of a duplicate", func(ctx context.Context, s *Store) error { _, err := s.Apply(ctx, base); return err }},
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
		{"Unmerges", func(ctx context.Context, s *Store) error { _, err := s.Unmerges(ctx, id, time.Time{}); return err }},
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
		{"Apply of many rows", func(ctx context.Context, s *Store) error { _, err := s.Apply(ctx, next(rowChunk+1)); return err }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			counter := &faultPool{}
			if err := op.do(ctx, withPool(s, counter)); err != nil {
				t.Fatal(err)
			}
			for at := 1; at <= counter.calls; at++ {
				headBefore, _ := s.Head(ctx)
				f := &faultPool{failAt: at}
				if err := op.do(ctx, withPool(s, f)); !errors.Is(err, errInjected) {
					t.Fatalf("statement %d of %d: got %v, want the injected failure", at, counter.calls, err)
				}
				if got, err := s.Head(ctx); err != nil || !got.Equal(headBefore) {
					t.Fatalf("statement %d: head is %v, %v after the failure, want %v", at, got, err, headBefore)
				}
				if err := op.do(ctx, s); err != nil {
					t.Fatalf("statement %d: retry after failure: %v", at, err)
				}
			}
		})
	}
}

// A restore that fails at any statement leaves a store that Restore can fill:
// empty, or marked as restoring.
func TestRestoreFailingAtAnyStatementCanBeRetried(t *testing.T) {
	t.Parallel()
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
	want, _ := src.Head(ctx)

	dst, _ := graphStoreAt(t)
	counter := &faultPool{}
	if err := withPool(dst, counter).Restore(ctx, bytes.NewReader(backup)); err != nil {
		t.Fatal(err)
	}
	total := counter.calls
	if got, _ := dst.Head(ctx); !got.Equal(want) {
		t.Fatalf("got head %v, want %v", got, want)
	}
	for at := 1; at <= total; at++ {
		fresh, _ := graphStoreAt(t)
		f := &faultPool{failAt: at}
		if err := withPool(fresh, f).Restore(ctx, bytes.NewReader(backup)); err == nil {
			t.Fatalf("statement %d of %d: Restore succeeded despite the failure", at, total)
		}
		if err := fresh.Restore(ctx, bytes.NewReader(backup)); err != nil {
			t.Fatalf("statement %d of %d: Restore after the failure: %v", at, total, err)
		}
		if got, _ := fresh.Head(ctx); !got.Equal(want) {
			t.Fatalf("statement %d: got head %v, want %v", at, got, want)
		}
	}
}

// Opening a store survives a failure at any step of the migrations.
func TestOpenFailingAtAnyStatementCanBeRetried(t *testing.T) {
	ctx := context.Background()
	s, o := openTestStore(t)
	schema := o.Schema
	counter := &faultPool{pool: s.db}
	if _, err := New(ctx, counter, o.User, Options{Schema: schema, AllowSuperuser: true}); err != nil {
		t.Fatal(err)
	}
	for at := 1; at <= counter.calls; at++ {
		f := &faultPool{pool: s.db, failAt: at}
		if _, err := New(ctx, f, o.User, Options{Schema: schema, AllowSuperuser: true}); !errors.Is(err, errInjected) {
			t.Fatalf("statement %d of %d: got %v, want the injected failure", at, counter.calls, err)
		}
		if _, err := New(ctx, s.db, o.User, Options{Schema: schema, AllowSuperuser: true}); err != nil {
			t.Fatalf("statement %d: open after the failure: %v", at, err)
		}
	}
}

// The database failing a transaction in a way another try can get through is
// retried; a store that can't get through gives up with ErrBusy; any other
// failure is returned at once.
func TestApplyRetriesTransientFailuresAndGivesUp(t *testing.T) {
	ctx := context.Background()
	s, _ := graphStoreAt(t)
	cs := func(event string) *modelv1alpha1.ChangeSet {
		head, _ := s.Head(ctx)
		return &modelv1alpha1.ChangeSet{EventId: event, BaseRecordedAt: timestamppb.New(head), State: stateEntries(1, event)}
	}
	serialization := &pgconn.PgError{Code: "40001", Message: "could not serialize access"}

	// One failed transaction, then success.
	once := &faultPool{failAt: 3, err: serialization}
	if _, err := withPool(s, once).Apply(ctx, cs("retried")); err != nil {
		t.Fatalf("got %v, want the retry to succeed", err)
	}
	// Always failing: the caller can tell the store was too busy.
	always := &alwaysFailing{err: serialization}
	_, err := withPool(s, &faultPool{}).applyWith(ctx, always, cs("busy"))
	if !errors.Is(err, ErrBusy) || !errors.Is(err, serialization) {
		t.Fatalf("got %v, want ErrBusy wrapping the cause", err)
	}
	if got := telemetry.ErrorType(err); got != "busy" {
		t.Fatalf("got error.type %q, want busy", got)
	}
	if always.n != maxApplyAttempts {
		t.Fatalf("tried %d times, want %d", always.n, maxApplyAttempts)
	}
	// A failure another try would not fix is not retried.
	permanent := &alwaysFailing{err: &pgconn.PgError{Code: "23505", Message: "duplicate key"}}
	if _, err := withPool(s, &faultPool{}).applyWith(ctx, permanent, cs("permanent")); err == nil || errors.Is(err, ErrBusy) || permanent.n != 1 {
		t.Fatalf("got %v after %d tries, want a failure at once", err, permanent.n)
	}
}

// alwaysFailing is a pool whose transactions cannot begin.
type alwaysFailing struct {
	err error
	n   int
}

func (a *alwaysFailing) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	a.n++
	return nil, a.err
}
func (a *alwaysFailing) Close() {}

// applyWith runs Apply against p instead of the store's pool.
func (s *Store) applyWith(ctx context.Context, p pool, cs *modelv1alpha1.ChangeSet) (contracts.ApplyResult, error) {
	return (&Store{db: p, Now: s.Now, IDs: s.IDs}).Apply(ctx, cs)
}

// A change to a row that has gone fails the apply, writes nothing, and says
// which statement did not do what it claimed (#95).
func TestAChangedVersionThatIsMissingFailsTheApply(t *testing.T) {
	ctx := context.Background()
	s, clk := graphStoreAt(t)
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "first", State: stateEntries(1, "k")}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	head, _ := s.Head(ctx)
	retract := &modelv1alpha1.ChangeSet{EventId: "second", BaseRecordedAt: timestamppb.New(head), State: []*modelv1alpha1.StateEntry{{Key: "k-00000"}}}
	gone := false
	f := &faultPool{before: func(ctx context.Context, sql string, tx pgx.Tx) {
		// Another writer's doing, which the head lock should make
		// impossible: the row the apply is about to change is gone.
		if strings.HasPrefix(sql, "UPDATE version") && !gone {
			gone = true
			if _, err := tx.Exec(ctx, `DELETE FROM version`); err != nil {
				t.Error(err)
			}
		}
	}}
	_, err := withPool(s, f).Apply(ctx, retract)
	if err == nil || !strings.Contains(err.Error(), "changed 0 of 1 rows") {
		t.Fatalf("got %v, want a refusal naming the missing row", err)
	}
	if got, _ := s.Head(ctx); !got.Equal(head) {
		t.Fatalf("the failed apply moved the head to %v", got)
	}
}

// A commit that landed but whose acknowledgement was lost is not applied
// twice: the retry finds the event and returns the original result.
func TestACommitWhoseAnswerWasLostIsNotAppliedTwice(t *testing.T) {
	ctx := context.Background()
	s, _ := graphStoreAt(t)
	first := &modelv1alpha1.ChangeSet{EventId: "lost-ack", State: stateEntries(2, "k")}
	// Count the statements of a clean apply to learn which one is the commit.
	counter := &faultPool{}
	if _, err := withPool(s, counter).Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "probe", State: stateEntries(1, "probe")}); err != nil {
		t.Fatal(err)
	}
	head, _ := s.Head(ctx)
	first.BaseRecordedAt = timestamppb.New(head)
	f := &faultPool{failAt: counter.calls, lostAck: true, err: &pgconn.PgError{Code: "08006", Message: "connection failure"}}
	res, err := withPool(s, f).Apply(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate {
		t.Fatal("the retry after the lost answer did not find the landed apply")
	}
	if n := count(t, s, `SELECT count(*) FROM journal WHERE event = 'lost-ack'`); n != 1 {
		t.Fatalf("got %d journal entries for the event, want 1", n)
	}
	if got, _ := s.Head(ctx); !got.Equal(res.RecordedAt) {
		t.Fatalf("got head %v, want the original apply's %v", got, res.RecordedAt)
	}
}
