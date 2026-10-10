package pgstore

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
)

// TestRandomWideHistoriesMatchTheReference adds conflicts, data-quality issues
// and merge reviews to the histories above, reads with predicate and object
// filters and on both axes of Changes, and ends by restoring a backup into
// a fresh store and comparing that too.
func TestRandomWideHistoriesMatchTheReference(t *testing.T) {
	t.Parallel()
	for seed := uint64(100); seed < 103; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			runWide(t, seed, 30)
		})
	}
}

func runWide(t *testing.T, seed uint64, steps int) {
	ctx := context.Background()
	start := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	clk := testkit.NewClock(start)
	pg := newTestStore(t)
	pg.Now, pg.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	ref := memstore.New()
	ref.Now, ref.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	h := &history{t: t, rng: rand.New(rand.NewPCG(seed, seed)), clk: clk, ref: ref, pg: pg} //nolint:gosec // G404: a seeded generator makes a failing history repeatable
	h.step(ctx, h.mintStep(5))
	for range steps {
		clk.Advance(time.Duration(1+h.rng.IntN(3)) * time.Minute)
		switch n := h.rng.IntN(100); {
		case n < 8:
			h.step(ctx, h.mintStep(1+h.rng.IntN(2)))
		case n < 25:
			h.step(ctx, h.claimStep())
		case n < 45:
			h.step(ctx, h.mergeStep(ctx))
		case n < 55:
			h.step(ctx, h.unmergeStep(ctx))
		case n < 62:
			h.step(ctx, h.rebindStep())
		case n < 72:
			h.step(ctx, conflictStep(h))
		case n < 82:
			h.step(ctx, issueStep(h))
		case n < 92:
			h.step(ctx, reviewStep(ctx, h))
		default:
			h.step(ctx, h.stateStep())
		}
		h.compare(ctx)
		compareWide(ctx, h)
	}
	cs, _ := ref.Conflicts(ctx, "", "", time.Time{}, time.Time{})
	dq, _ := ref.DataQuality(ctx, contracts.IssueFilter{}, time.Time{}, time.Time{})
	var buf bytes.Buffer
	if err := pg.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	dst := newTestStore(t)
	dst.Now, dst.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	if err := dst.Restore(ctx, &buf); err != nil {
		t.Fatalf("restore: %v", err)
	}
	h.pg = dst
	h.compare(ctx)
	compareWide(ctx, h)
	t.Logf("steps %d bad %d conflicts %d issues %d", h.steps, h.bad, len(cs), len(dq))
}

func conflictStep(h *history) *modelv1alpha1.ChangeSet {
	s := h.pick()
	pred := []string{"owned_by", "name"}[h.rng.IntN(2)]
	c := &modelv1alpha1.Conflict{SubjectId: s, Predicate: pred, Positions: []*modelv1alpha1.ConflictPosition{
		{SourceSystem: "a", Objects: []*modelv1alpha1.FactObject{{SubjectId: h.pick()}}},
		{SourceSystem: "b", Objects: []*modelv1alpha1.FactObject{{SubjectId: h.pick()}}},
	}}
	return &modelv1alpha1.ChangeSet{Conflicts: []*modelv1alpha1.ConflictTimeline{{SubjectId: s, Predicate: pred, Conflicts: []*modelv1alpha1.Conflict{c}}}}
}

func issueStep(h *history) *modelv1alpha1.ChangeSet {
	key := fmt.Sprintf("issue-%d", h.rng.IntN(3))
	return &modelv1alpha1.ChangeSet{Issues: []*modelv1alpha1.IssueTimeline{{Key: key, Spans: []*modelv1alpha1.IssueSpan{{Issue: &modelv1alpha1.DataQualityIssue{
		Issue: modelv1alpha1.IssueType_ISSUE_TYPE_ID_CONFLICT, SubjectIds: []string{h.pick(), h.pick()},
		Supports: []*modelv1alpha1.Support{{Source: "github-acme"}},
	}}}}}}
}

func reviewStep(ctx context.Context, h *history) *modelv1alpha1.ChangeSet {
	id := h.pick()
	score := uint32(h.rng.IntN(3)) * 1000 //nolint:gosec // G115: below 3000
	ms, _ := h.ref.Merges(ctx, contracts.SubjectID(id), time.Time{})
	for _, m := range ms {
		if m.GetMergedId() == id {
			return &modelv1alpha1.ChangeSet{MergeReviews: []*modelv1alpha1.MergeReviewWrite{{
				SubjectId: id, MergeEventId: m.GetEventId(),
				Review: &modelv1alpha1.MergeReview{Status: modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_HOLDS, SurvivorScorePpm: score},
			}}}
		}
	}
	return nil
}

func compareWide(ctx context.Context, h *history) {
	h.t.Helper()
	times := []time.Time{{}}
	for i := 0; i < 3 && len(h.heads) > 0; i++ {
		times = append(times, h.heads[h.rng.IntN(len(h.heads))])
	}
	for _, r := range times {
		for i := 0; i < 3; i++ {
			id := h.pick()
			sid := contracts.SubjectID(id)
			for _, pred := range []string{"", "owned_by", "name"} {
				h.same("Conflicts pred", r, id+pred, func(s contracts.GraphStore) (any, error) { return s.Conflicts(ctx, sid, pred, time.Time{}, r) })
				h.same("Conflicts nosubject", r, pred, func(s contracts.GraphStore) (any, error) { return s.Conflicts(ctx, "", pred, time.Time{}, r) })
				h.same("Supports pred", r, id+pred, func(s contracts.GraphStore) (any, error) {
					return s.Supports(ctx, contracts.SupportFilter{SubjectID: sid, Predicate: pred}, r)
				})
				h.same("Supports pred only", r, pred, func(s contracts.GraphStore) (any, error) {
					return s.Supports(ctx, contracts.SupportFilter{Predicate: pred}, r)
				})
				h.same("AsOf subj pred", r, id+pred, func(s contracts.GraphStore) (any, error) {
					return s.AsOf(ctx, contracts.FactFilter{SubjectID: sid, Predicate: pred}, time.Time{}, r)
				})
				h.same("AsOf obj pred", r, id+pred, func(s contracts.GraphStore) (any, error) {
					return s.AsOf(ctx, contracts.FactFilter{Object: &modelv1alpha1.FactObject{SubjectId: id}, Predicate: pred}, time.Time{}, r)
				})
				h.same("AsOf subj obj", r, id+pred, func(s contracts.GraphStore) (any, error) {
					return s.AsOf(ctx, contracts.FactFilter{SubjectID: sid, Object: &modelv1alpha1.FactObject{SubjectId: h.subjects[0]}, Predicate: pred}, time.Time{}, r)
				})
			}
			h.same("Changes valid", r, id, func(s contracts.GraphStore) (any, error) {
				return s.Changes(ctx, contracts.FactFilter{SubjectID: sid}, h.clk.Now().Add(-time.Hour), h.clk.Now().Add(time.Hour), contracts.AxisValid)
			})
			h.same("Changes record obj", r, id, func(s contracts.GraphStore) (any, error) {
				return s.Changes(ctx, contracts.FactFilter{Object: &modelv1alpha1.FactObject{SubjectId: id}}, h.heads[0], h.clk.Now().Add(time.Hour), contracts.AxisRecord)
			})
			h.same("Changes record mid", r, id, func(s contracts.GraphStore) (any, error) {
				return s.Changes(ctx, contracts.FactFilter{SubjectID: sid}, h.heads[len(h.heads)/2], h.heads[len(h.heads)-1], contracts.AxisRecord)
			})
		}
		h.same("DataQuality all", r, "", func(s contracts.GraphStore) (any, error) {
			return s.DataQuality(ctx, contracts.IssueFilter{}, time.Time{}, r)
		})
		h.same("DataQuality kind", r, "", func(s contracts.GraphStore) (any, error) {
			return s.DataQuality(ctx, contracts.IssueFilter{Kinds: []string{"Team"}}, time.Time{}, r)
		})
		h.same("DataQuality kind none", r, "", func(s contracts.GraphStore) (any, error) {
			return s.DataQuality(ctx, contracts.IssueFilter{Kinds: []string{"Repository"}}, time.Time{}, r)
		})
	}
}
