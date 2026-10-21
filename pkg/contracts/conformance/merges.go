package conformance

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// The cases of GraphStore for merge reviews and un-merge records
// (docs/spec/contracts.md, "GraphStore").

func (g *suite) mergeReviews(t *testing.T) {
	s, clk := g.store(t)
	_, p, l := seed(t, s)
	before, _ := s.Head(ctx)
	review := func(survivor, merged uint32, status modelv1alpha1.MergeReviewStatus) *modelv1alpha1.MergeReview {
		return &modelv1alpha1.MergeReview{SurvivorScorePpm: survivor, MergedScorePpm: merged, Status: status}
	}
	holds, needs := modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_HOLDS, modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_NEEDS_REVIEW
	write := func(subject, event string, rv *modelv1alpha1.MergeReview) []*modelv1alpha1.MergeReviewWrite {
		return []*modelv1alpha1.MergeReviewWrite{{SubjectId: subject, MergeEventId: event, Review: rv}}
	}
	// reviews returns the reviews of the merge of l into p as recorded at at.
	reviews := func(at time.Time) []*modelv1alpha1.MergeReview {
		recs, err := s.Merges(ctx, contracts.SubjectID(p), at)
		failIf(t, err != nil, "%v", err)
		for _, rec := range recs {
			if rec.GetMergedId() == l {
				return rec.GetReviews()
			}
		}
		return nil
	}
	// A merge and its first review land together.
	res := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:      "e2",
		Merges:       []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_SCORE, ConfidencePpm: 950_000}},
		MergeReviews: write(l, "e2", review(950_000, 910_000, holds)),
	})
	first := res.RecordedAt
	got := reviews(time.Time{})
	failIf(t, len(got) != 1 || got[0].GetSurvivorScorePpm() != 950_000 || got[0].GetMergedScorePpm() != 910_000 || got[0].GetStatus() != holds || got[0].GetEventId() != "e2" || !got[0].GetRecordedAt().AsTime().Equal(first), "got %v, want the review e2 wrote", got)
	// The same findings again change nothing, so a resolver that re-evaluates
	// on every apply does not grow the record.
	clk.Set(clk.Now().Add(time.Hour))
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e3", MergeReviews: write(l, "e2", review(950_000, 910_000, holds))})
	got = reviews(time.Time{})
	failIf(t, len(got) != 1, "got %d reviews, want 1 after the same findings were written again", len(got))
	// New findings add a review, and earlier record times keep seeing the old.
	clk.Set(clk.Now().Add(time.Hour))
	second := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e4", MergeReviews: write(l, "e2", review(950_000, 300_000, needs))}).RecordedAt
	got = reviews(time.Time{})
	failIf(t, len(got) != 2 || got[1].GetStatus() != needs || got[1].GetMergedScorePpm() != 300_000 || got[1].GetEventId() != "e4" || !got[1].GetRecordedAt().AsTime().Equal(second), "got %v, want the review history ending in e4's", got)
	got = reviews(first)
	failIf(t, len(got) != 1 || got[0].GetStatus() != holds, "got %v, want only e2's review as recorded at %s", got, first)
	got = reviews(first.Add(-time.Microsecond))
	failIf(t, len(got) != 0, "got %v, want no reviews before the merge", got)
	recs, _ := s.Merges(ctx, contracts.SubjectID(p), before)
	failIf(t, len(recs) != 0, "got %v, want no record before the merge", recs)
	// A subject that merges in the same event can be named by its ref.
	clk.Set(clk.Now().Add(time.Hour))
	res = apply(t, s, &modelv1alpha1.ChangeSet{
		EventId: "e5", Mints: []*modelv1alpha1.Mint{mint("new:t", "Team")},
		Merges:       []*modelv1alpha1.Merge{{SubjectIds: []string{p, "new:t"}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE}},
		MergeReviews: write("new:t", "e5", review(0, 0, holds)),
	})
	recs, _ = s.Merges(ctx, res.Subjects["new:t"], time.Time{})
	failIf(t, len(recs) != 1 || len(recs[0].GetReviews()) != 1, "got %v, want the merge of %s with its review", recs, res.Subjects["new:t"])
	head, _ := s.Head(ctx)
	for name, rws := range map[string][]*modelv1alpha1.MergeReviewWrite{
		"no review":                write(l, "e2", nil),
		"no status":                write(l, "e2", review(1, 1, modelv1alpha1.MergeReviewStatus_MERGE_REVIEW_STATUS_UNSPECIFIED)),
		"unknown status":           write(l, "e2", review(1, 1, 999)),
		"survivor score too big":   write(l, "e2", review(1_000_001, 1, holds)),
		"merged score too big":     write(l, "e2", review(1, 1_000_001, holds)),
		"no such event":            write(l, "nope", review(1, 1, holds)),
		"the surviving subject":    write(p, "e2", review(1, 1, holds)),
		"unknown subject":          write("0192b1c4-0000-7000-8000-000000000000", "e2", review(1, 1, holds)),
		"unknown ref":              write("new:nope", "e2", review(1, 1, holds)),
		"two reviews of one merge": append(write(l, "e2", review(2, 2, needs)), write(l, "e2", review(3, 3, needs))...),
	} {
		_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad/" + name, MergeReviews: rws})
		failIf(t, err == nil, "%s: got no error, want one", name)
	}
	now, _ := s.Head(ctx)
	failIf(t, !now.Equal(head), "got head %s, want %s: a refused review wrote", now, head)
	got = reviews(time.Time{})
	failIf(t, len(got) != 2, "got %d reviews, want the 2 written before", len(got))
}

func (g *suite) unmergeRecords(t *testing.T) {
	s, clk := g.store(t)
	_, p, l := seed(t, s)
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e1", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{p, l}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_MANUAL}}})
	merged, _ := s.Head(ctx)
	unmerges := func(id string, at time.Time) []*modelv1alpha1.UnmergeRecord {
		recs, err := s.Unmerges(ctx, contracts.SubjectID(id), at)
		failIf(t, err != nil, "%v", err)
		return recs
	}
	recs := unmerges(p, time.Time{})
	failIf(t, len(recs) != 0, "got %v, want no un-merge records yet", recs)
	// A reactivation.
	clk.Set(clk.Now().Add(time.Hour))
	revive := apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_l"}, Ref: "new:back"}}})
	want := &modelv1alpha1.UnmergeRecord{
		SubjectId: p, TargetId: l, Aliases: []string{"github:team_node/T_l"}, EventId: "e2", RecordedAt: timestamppb.New(revive.RecordedAt), MergeEventId: "e1",
	}
	for _, id := range []string{p, l} {
		recs := unmerges(id, time.Time{})
		failIf(t, len(recs) != 1 || !proto.Equal(recs[0], want), "un-merges of %s: got %v, want %v", id, recs, want)
	}
	recs = unmerges(p, merged)
	failIf(t, len(recs) != 0, "got %v, want none as recorded before the un-merge", recs)
	// A split, which leaves no other trace of where its subject came from.
	clk.Set(clk.Now().Add(time.Hour))
	apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e3", Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row(p, "", ""))}})
	clk.Set(clk.Now().Add(time.Hour))
	split := apply(t, s, &modelv1alpha1.ChangeSet{
		EventId:  "e4",
		Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team/acme/payments"}, Ref: "new:split"}},
		Bindings: []*modelv1alpha1.BindingTimeline{bind("github:team/acme/payments", row("new:split", "", ""))},
	})
	id := string(split.Subjects["new:split"])
	wantSplit := &modelv1alpha1.UnmergeRecord{
		SubjectId: p, TargetId: id, Split: true, Aliases: []string{"github:team/acme/payments"}, EventId: "e4", RecordedAt: timestamppb.New(split.RecordedAt),
	}
	recs = unmerges(id, time.Time{})
	failIf(t, len(recs) != 1 || !proto.Equal(recs[0], wantSplit), "un-merges of the split subject: got %v, want %v", recs, wantSplit)
	recs = unmerges(p, time.Time{})
	failIf(t, len(recs) != 2 || !proto.Equal(recs[0], want) || !proto.Equal(recs[1], wantSplit), "un-merges of %s: got %v, want the reactivation and then the split", p, recs)
	recs = unmerges(p, revive.RecordedAt)
	failIf(t, len(recs) != 1 || !proto.Equal(recs[0], want), "got %v, want only the reactivation as recorded at %s", recs, revive.RecordedAt)
	// A refused un-merge leaves no record.
	_, err := tryApply(s, &modelv1alpha1.ChangeSet{EventId: "bad", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: p, Aliases: []string{"github:team_node/T_p"}, Ref: "new:x"}}})
	failIf(t, err == nil, "%v", "un-merged all of a subject's aliases")
	recs = unmerges(p, time.Time{})
	failIf(t, len(recs) != 2, "got %v, want the 2 records written before", recs)
}

// chainUnmerge is the decision recorded on issue 54: an un-merge through a
// chain of merges mints a split, and an exact undo takes one step per merge.
func (g *suite) chainUnmerge(t *testing.T) {
	// Z < A < B. B merges into A, and A into Z.
	chain := func(t *testing.T) (s contracts.GraphStore, z, a, b string) {
		s, _ = g.store(t)
		res := apply(t, s, &modelv1alpha1.ChangeSet{
			EventId: "e1", Mints: []*modelv1alpha1.Mint{mint("new:z", "Person"), mint("new:a", "Person"), mint("new:b", "Person")},
			Bindings: []*modelv1alpha1.BindingTimeline{
				bind("directory:user/z", row("new:z", "", "")), bind("directory:user/a", row("new:a", "", "")), bind("github:user_node/b", row("new:b", "", "")),
			},
		})
		z, a, b = string(res.Subjects["new:z"]), string(res.Subjects["new:a"]), string(res.Subjects["new:b"])
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e2", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{a, b}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE}}})
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e3", Merges: []*modelv1alpha1.Merge{{SubjectIds: []string{z, a}, Rule: modelv1alpha1.MergeRule_MERGE_RULE_AUTHORITATIVE}}})
		return s, z, a, b
	}
	status := func(t *testing.T, s contracts.GraphStore, id string) *modelv1alpha1.Subject {
		t.Helper()
		sub, err := s.Subject(ctx, contracts.SubjectID(id), time.Time{})
		failIf(t, err != nil, "%v", err)
		return sub
	}
	t.Run("naming B's aliases on Z mints a split and leaves B merged", func(t *testing.T) {
		s, z, _, b := chain(t)
		res := apply(t, s, &modelv1alpha1.ChangeSet{
			EventId:  "e4",
			Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: z, Aliases: []string{"github:user_node/b"}, Ref: "new:split"}},
			Bindings: []*modelv1alpha1.BindingTimeline{bind("github:user_node/b", row("new:split", "", ""))},
		})
		split := string(res.Subjects["new:split"])
		sub := status(t, s, split)
		failIf(t, sub.GetMintedBy().GetRule() != modelv1alpha1.MintRule_MINT_RULE_SPLIT || sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want an active subject minted by split", sub)
		sub = status(t, s, b)
		failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_MERGED, "got %v, want B still merged", sub)
		recs, _ := s.Unmerges(ctx, contracts.SubjectID(z), time.Time{})
		failIf(t, len(recs) != 1 || !recs[0].GetSplit() || recs[0].GetTargetId() != split, "got %v, want one split record", recs)
	})
	t.Run("two steps restore B exactly", func(t *testing.T) {
		s, z, a, b := chain(t)
		// A leaves Z with the aliases it held when it merged, B's included.
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e4", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: z, Aliases: []string{"directory:user/a", "github:user_node/b"}, Ref: "new:a"}}})
		sub := status(t, s, a)
		failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want A active again", sub)
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: "e5", Unmerges: []*modelv1alpha1.Unmerge{{SubjectId: a, Aliases: []string{"github:user_node/b"}, Ref: "new:b"}}})
		sub = status(t, s, b)
		failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want B active again", sub)
		for _, id := range []string{z, a, b} {
			sub := status(t, s, id)
			failIf(t, sub.GetStatus() != modelv1alpha1.SubjectStatus_SUBJECT_STATUS_ACTIVE, "got %v, want %s active", sub, id)
		}
	})
}
