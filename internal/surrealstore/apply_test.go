package surrealstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"

	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
)

// sleepQ delays the second staging transaction of an apply inside the
// transaction, after it has read the head, and runs during(), which
// commits another apply, while it waits.
type sleepQ struct {
	Querier
	mu     sync.Mutex
	stages int
	during func()
}

func (q *sleepQ) Query(ctx context.Context, sql string, vars map[string]any) ([]any, error) {
	if strings.Contains(sql, "UPDATE meta:graph SET staged") {
		q.mu.Lock()
		q.stages++
		second := q.stages == 2
		q.mu.Unlock()
		if second {
			done := make(chan struct{})
			go func() {
				time.Sleep(400 * time.Millisecond) // the transaction has begun by now
				q.during()
				close(done)
			}()
			sql = strings.Replace(sql, "COMMIT TRANSACTION;", "sleep 1500ms; COMMIT TRANSACTION;", 1)
			res, err := q.Querier.Query(ctx, sql, vars)
			<-done
			return res, err
		}
	}
	return q.Querier.Query(ctx, sql, vars)
}

// A commit that lands while a later staging transaction of a large apply is
// running must make that transaction fail, not leave its rows behind the
// head where every reader sees them.
func TestCommitDuringALaterStageTransactionLeavesNoOrphans(t *testing.T) {
	ctx := context.Background()
	// The test holds a transaction open with sleep, which only root may call.
	clk := testkit.NewClock(time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC))
	s := newTestStoreAs(t, false)
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	other := graphStore(t, s, clk)
	q := &sleepQ{Querier: s.q, during: func() {
		if _, err := other.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "small", State: stateEntries(1, "small")}); err != nil {
			t.Error(err)
		}
	}}
	slow := withQ(s, q)
	// Four rows per entry and more than stageRows of them: three staging transactions.
	const n = inlineRows
	big := &modelv1alpha1.ChangeSet{EventId: "big", State: stateEntries(n, "big")}
	// The head moved under it, so it reports that and nothing of it shows.
	if _, err := slow.Apply(ctx, big); !errors.Is(err, contracts.ErrStale) {
		t.Fatalf("got %v, want ErrStale", err)
	}
	if q.stages < 2 {
		t.Fatalf("only %d staging transactions ran", q.stages)
	}
	if st, err := s.State(ctx, []string{"big-00000", fmt.Sprintf("big-%05d", n-1)}, time.Time{}); err != nil || len(st) != 0 {
		t.Fatalf("got %d keys of the lost apply, %v, want none", len(st), err)
	}
	// Applied against the new head it commits, once per series.
	head, _ := s.Head(ctx)
	big.BaseRecordedAt = timestamppb.New(head)
	if _, err := s.Apply(ctx, big); err != nil {
		t.Fatal(err)
	}
	keys := []string{"big-00000", fmt.Sprintf("big-%05d", n-1), "small-00000"}
	st, err := s.State(ctx, keys, time.Time{})
	if err != nil || len(st) != 3 {
		t.Fatalf("got %d keys, %v, want all three", len(st), err)
	}
	// Every big-… series holds exactly the one row of the one attempt that landed.
	if got := count(t, s, `SELECT count() AS n FROM version WHERE key CONTAINS 'big-' GROUP ALL`, nil); got != n {
		t.Fatalf("got %d version rows of the large apply, want %d", got, n)
	}
}

func facts(subject, object string, confidencePpm uint32, clk func() time.Time) ([]*modelv1alpha1.SupportTimeline, []*modelv1alpha1.FactTimeline) {
	o := &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue(object)}
	return []*modelv1alpha1.SupportTimeline{{
		Source: "github-acme", SubjectId: subject, Predicate: "name", Object: o,
		Versions: []*modelv1alpha1.Support{{Source: "github-acme", ConfidencePpm: proto.Uint32(confidencePpm), Reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT, EventId: "x", ObservedAt: timestamppb.New(clk())}},
	}}, []*modelv1alpha1.FactTimeline{{
		SubjectId: subject, Predicate: "name", Object: o,
		Spans: []*modelv1alpha1.FactSpan{{Status: modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, StatusReason: modelv1alpha1.StatusReason_STATUS_REASON_NONE, ConfidencePpm: confidencePpm}},
	}}
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
		want, err := m.Apply(ctx, cloneChangeSet(cs))
		if err != nil {
			t.Fatalf("%s: reference: %v", cs.GetEventId(), err)
		}
		got, err := s.Apply(ctx, cs)
		if err != nil {
			t.Fatalf("%s: %v", cs.GetEventId(), err)
		}
		return got, want
	}
	sup, fct := facts("new:l", "payments", 800_000, sclk.Now)
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

	sup, fct = facts("new:back", "payments", 900_000, sclk.Now)
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
	if n := count(t, s, `SELECT count() AS n FROM series WHERE tbl = $t GROUP ALL`, map[string]any{"t": int(memstore.TableFacts)}); n != 1 {
		t.Fatalf("got %d fact series, want the one that exists", n)
	}
}

func cloneChangeSet(cs *modelv1alpha1.ChangeSet) *modelv1alpha1.ChangeSet {
	c, _ := proto.Clone(cs).(*modelv1alpha1.ChangeSet) // always a ChangeSet
	return c
}
