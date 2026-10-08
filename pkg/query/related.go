package query

import (
	"context"
	"fmt"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// Relations are the relation facts a subject takes part in at a point.
type Relations struct {
	Point   Point `json:"point"`
	Subject Ref   `json:"subject"`
	// Out are relations whose subject it is: it is a member_of Engineering.
	Out []Fact `json:"out"`
	// In are relations whose object it is: Jane Doe is a member_of it.
	In []Fact `json:"in"`
}

// Related returns the subject's relations in both directions, of any status
// but none. A predicate limits them to that relation.
func (q *Querier) Related(ctx context.Context, ref, predicate string, p Point) (*Relations, error) {
	id, err := q.Resolve(ctx, ref, p)
	if err != nil {
		return nil, err
	}
	l := q.labeler(p)
	subject, err := l.ref(ctx, string(id))
	if err != nil {
		return nil, err
	}
	r := &Relations{Point: p, Subject: subject, Out: []Fact{}, In: []Fact{}}
	out, err := q.Graph.AsOf(ctx, contracts.FactFilter{SubjectID: id, Predicate: predicate}, p.Valid, p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("relations from %s: %w", id, err)
	}
	in, err := q.Graph.AsOf(ctx, contracts.FactFilter{Predicate: predicate, Object: &modelv1alpha1.FactObject{SubjectId: string(id)}}, p.Valid, p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("relations to %s: %w", id, err)
	}
	for _, s := range out {
		if s.GetObject().GetSubjectId() == "" {
			continue // an attribute, not a relation
		}
		f, err := l.fact(ctx, s)
		if err != nil {
			return nil, err
		}
		r.Out = append(r.Out, f)
	}
	for _, s := range in {
		f, err := l.fact(ctx, s)
		if err != nil {
			return nil, err
		}
		r.In = append(r.In, f)
	}
	return r, nil
}
