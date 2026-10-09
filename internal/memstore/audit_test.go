package memstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// twoTeams mints two teams, each with an alias, merges them (manual) in a
// second apply and returns the survivor and the merged one.
func twoTeams(t *testing.T, s *Store, clk interface{ Advance(time.Duration) }) (survivor, merged string) {
	t.Helper()
	apply := func(cs *modelv1alpha1.ChangeSet) contracts.ApplyResult {
		t.Helper()
		if head, _ := s.Head(context.Background()); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		clk.Advance(time.Second)
		res, err := s.Apply(context.Background(), cs)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := apply(&modelv1alpha1.ChangeSet{
		EventId: "e1", Mints: []*modelv1alpha1.Mint{mintRef("new:a", "Team"), mintRef("new:b", "Team")},
		Bindings: []*modelv1alpha1.BindingTimeline{
			{Alias: "github:team_node/A", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/A", SubjectId: "new:a"}}},
			{Alias: "github:team_node/B", Bindings: []*modelv1alpha1.Binding{{Alias: "github:team_node/B", SubjectId: "new:b"}}},
		},
	})
	survivor, merged = string(res.Subjects["new:a"]), string(res.Subjects["new:b"])
	apply(&modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{survivor, merged}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE}}})
	return survivor, merged
}

// TestMergeReviewsAtTheTimelineLimitReplaceTheNewest: a merge record keeps at
// most MaxTimelineRows reviews, and a resolver that flips its findings every
// apply is not refused for it, so an input that toggles evidence cannot wedge
// the events that touch the merge.
func TestMergeReviewsAtTheTimelineLimitReplaceTheNewest(t *testing.T) {
	s, clk := newTestStore()
	survivor, merged := twoTeams(t, s, clk)
	review := func(i uint32) *modelv1alpha1.ChangeSet {
		head, _ := s.Head(context.Background())
		return &modelv1alpha1.ChangeSet{
			EventId: fmt.Sprintf("r%d", i), BaseRecordedAt: timestamppb.New(head),
			MergeReviews: []*modelv1alpha1.MergeReviewWrite{{SubjectId: merged, MergeEventId: "e2", Review: &modelv1alpha1.MergeReview{
				MergedScorePpm: i, Status: modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_HOLDS,
			}}},
		}
	}
	for i := uint32(1); i <= contracts.MaxTimelineRows+10; i++ {
		clk.Advance(time.Second)
		if _, err := s.Apply(context.Background(), review(i)); err != nil {
			t.Fatalf("review %d: %v", i, err)
		}
	}
	recs, _ := s.Merges(context.Background(), contracts.SubjectID(survivor), time.Time{})
	if len(recs) != 1 || len(recs[0].GetReviews()) != contracts.MaxTimelineRows {
		t.Fatalf("got %v, want %d reviews kept", recs, contracts.MaxTimelineRows)
	}
	rs := recs[0].GetReviews()
	if first, last := rs[0].GetMergedScorePpm(), rs[len(rs)-1].GetMergedScorePpm(); first != 1 || last != contracts.MaxTimelineRows+10 {
		t.Fatalf("got reviews from %d to %d, want the first and the latest", first, last)
	}
}

// TestFailedAppliesLeaveNoUnmergeRecords: the un-merge records and their
// index and a merge's reviews roll back with the apply that wrote them.
func TestFailedAppliesLeaveNoUnmergeRecords(t *testing.T) {
	s, clk := newTestStore()
	survivor, merged := twoTeams(t, s, clk)
	head, _ := s.Head(context.Background())
	clk.Advance(time.Second)
	_, err := s.Apply(context.Background(), &modelv1alpha1.ChangeSet{
		EventId: "bad", BaseRecordedAt: timestamppb.New(head),
		Unmerges:     []*modelv1alpha1.Unmerge{{SubjectId: survivor, Aliases: []string{"github:team_node/B"}, Ref: "new:b"}},
		MergeReviews: []*modelv1alpha1.MergeReviewWrite{{SubjectId: merged, MergeEventId: "e2", Review: &modelv1alpha1.MergeReview{Status: modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_HOLDS}}},
		State:        []*modelv1alpha1.StateEntry{{}}, // refused after the rest was written
	})
	if err == nil {
		t.Fatal("got no error from the bad apply")
	}
	if len(s.unmerges) != 0 || len(s.unmergesBy) != 0 {
		t.Fatalf("got %d un-merge records and %d index entries after a failed apply, want none", len(s.unmerges), len(s.unmergesBy))
	}
	if recs, _ := s.Merges(context.Background(), contracts.SubjectID(survivor), time.Time{}); len(recs) != 1 || recs[0].GetUnmergedAt() != nil || len(recs[0].GetReviews()) != 0 {
		t.Fatalf("got %v, want the merge record as it was", recs)
	}
}
