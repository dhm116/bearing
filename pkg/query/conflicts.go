package query

import (
	"context"
	"fmt"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// conflicts lists the conflicts on the subject (and predicate, if not
// empty) at the point.
func (q *Querier) conflicts(ctx context.Context, l *labeler, id contracts.SubjectID, predicate string) ([]Conflict, error) {
	in, err := q.Graph.Conflicts(ctx, id, predicate, l.p.Valid, l.p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("conflicts on %s: %w", id, err)
	}
	out := []Conflict{}
	for _, c := range in {
		subject, err := l.ref(ctx, c.GetSubjectId())
		if err != nil {
			return nil, err
		}
		cf := Conflict{
			Subject: subject, Predicate: c.GetPredicate(), ValidFrom: optTime(c.GetValidFrom()), ValidTo: optTime(c.GetValidTo()),
			Resolution: enumName("CONFLICT_RESOLUTION_", c.GetResolution().String()),
		}
		for _, pos := range c.GetPositions() {
			position := Position{SourceSystem: pos.GetSourceSystem(), Authoritative: pos.GetAuthority().GetAuthoritative()}
			for _, o := range pos.GetObjects() {
				obj, err := l.object(ctx, o)
				if err != nil {
					return nil, err
				}
				position.Objects = append(position.Objects, obj)
			}
			cf.Positions = append(cf.Positions, position)
		}
		out = append(out, cf)
	}
	return out, nil
}

// merges lists the merges involving the subject, oldest first.
func (q *Querier) merges(ctx context.Context, l *labeler, id contracts.SubjectID) ([]Merge, error) {
	in, err := q.Graph.Merges(ctx, id, l.p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("merges of %s: %w", id, err)
	}
	out := []Merge{}
	for _, m := range in {
		survivor, err := l.ref(ctx, m.GetSurvivorId())
		if err != nil {
			return nil, err
		}
		merged, err := l.ref(ctx, m.GetMergedId())
		if err != nil {
			return nil, err
		}
		out = append(out, Merge{
			Survivor: survivor, Merged: merged, Rule: enumName("MERGE_RULE_", m.GetRule().String()), ConfidencePPM: m.GetConfidencePpm(),
			EventID: m.GetEventId(), RecordedAt: m.GetRecordedAt().AsTime(), UnmergedAt: optTime(m.GetUnmergedAt()),
			Review: latestReview(m.GetReviews()),
		})
	}
	return out, nil
}

// latestReview returns the last of a merge's reviews, or nil.
func latestReview(reviews []*modelv1alpha1.MergeReview) *Review {
	if len(reviews) == 0 {
		return nil
	}
	rv := reviews[len(reviews)-1]
	return &Review{
		Status: enumName("MERGE_REVIEW_STATUS_", rv.GetStatus().String()), SurvivorScorePPM: rv.GetSurvivorScorePpm(), MergedScorePPM: rv.GetMergedScorePpm(),
		EventID: rv.GetEventId(), RecordedAt: rv.GetRecordedAt().AsTime(),
	}
}

// unmerges lists the un-merges involving the subject, oldest first.
func (q *Querier) unmerges(ctx context.Context, l *labeler, id contracts.SubjectID) ([]Unmerge, error) {
	in, err := q.Graph.Unmerges(ctx, id, l.p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("un-merges of %s: %w", id, err)
	}
	var out []Unmerge
	for _, u := range in {
		subject, err := l.ref(ctx, u.GetSubjectId())
		if err != nil {
			return nil, err
		}
		target, err := l.ref(ctx, u.GetTargetId())
		if err != nil {
			return nil, err
		}
		out = append(out, Unmerge{
			Subject: subject, Target: target, Split: u.GetSplit(), Aliases: u.GetAliases(), EventID: u.GetEventId(), RecordedAt: u.GetRecordedAt().AsTime(),
		})
	}
	return out, nil
}
