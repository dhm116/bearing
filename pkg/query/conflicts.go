package query

import (
	"context"
	"fmt"

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
		})
	}
	return out, nil
}
