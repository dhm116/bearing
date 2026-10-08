package query

import (
	"context"
	"fmt"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// PredicateOwnedBy is the relation Owners reads.
const PredicateOwnedBy = "owned_by"

// Ownership is who owns a subject at a point.
type Ownership struct {
	Point   Point `json:"point"`
	Subject Ref   `json:"subject"`
	// Owners are the asserted owned_by facts, and the only answer to who
	// owns the subject.
	Owners []Fact `json:"owners"`
	// NotAsserted are owned_by facts with support that isn't asserted
	// (a candidate below the threshold, one in a conflict), to say why an
	// owner might be missing. They are not owners.
	NotAsserted []Fact     `json:"not_asserted"`
	Conflicts   []Conflict `json:"conflicts"`
}

// Owners returns who owns the subject ref names at p. Only asserted facts
// are owners (docs/spec/data-model.md, "Status"); the rest are listed apart.
func (q *Querier) Owners(ctx context.Context, ref string, p Point) (*Ownership, error) {
	id, err := q.Resolve(ctx, ref, p)
	if err != nil {
		return nil, err
	}
	l := q.labeler(p)
	subject, err := l.ref(ctx, string(id))
	if err != nil {
		return nil, err
	}
	states, err := q.Graph.AsOf(ctx, contracts.FactFilter{SubjectID: id, Predicate: PredicateOwnedBy}, p.Valid, p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("owners of %s: %w", id, err)
	}
	o := &Ownership{Point: p, Subject: subject, Owners: []Fact{}, NotAsserted: []Fact{}}
	for _, s := range states {
		f, err := l.fact(ctx, s)
		if err != nil {
			return nil, err
		}
		if s.GetStatus() == modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED {
			o.Owners = append(o.Owners, f)
		} else {
			o.NotAsserted = append(o.NotAsserted, f)
		}
	}
	if o.Conflicts, err = q.conflicts(ctx, l, id, PredicateOwnedBy); err != nil {
		return nil, err
	}
	return o, nil
}
