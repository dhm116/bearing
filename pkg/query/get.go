package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// Key is an alias bound to a subject over a stretch of valid time.
type Key struct {
	Alias string `json:"alias"`
	// Redirect is set for a name the subject no longer goes by: it was
	// released, and still leads here.
	Redirect  bool       `json:"redirect,omitempty"`
	ValidFrom *time.Time `json:"valid_from,omitempty"`
	ValidTo   *time.Time `json:"valid_to,omitempty"`
}

// Merge is a merge record involving a subject.
type Merge struct {
	Survivor      Ref        `json:"survivor"`
	Merged        Ref        `json:"merged"`
	Rule          string     `json:"rule"`
	ConfidencePPM uint32     `json:"confidence_ppm"`
	EventID       string     `json:"event_id"`
	RecordedAt    time.Time  `json:"recorded_at"`
	UnmergedAt    *time.Time `json:"unmerged_at,omitempty"`
	// Review is the latest re-evaluation of the merge's evidence, if any.
	Review *Review `json:"review,omitempty"`
}

// Review is a re-evaluation of a merge's evidence (docs/spec/data-model.md,
// "Merge" step 4). The scores are set for score merges.
type Review struct {
	Status           string    `json:"status"`
	SurvivorScorePPM uint32    `json:"survivor_score_ppm,omitempty"`
	MergedScorePPM   uint32    `json:"merged_score_ppm,omitempty"`
	EventID          string    `json:"event_id"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// Unmerge is an un-merge record involving a subject: aliases that left one
// subject for another. A split minted the target subject.
type Unmerge struct {
	Subject    Ref       `json:"subject"`
	Target     Ref       `json:"target"`
	Split      bool      `json:"split,omitempty"`
	Aliases    []string  `json:"aliases"`
	EventID    string    `json:"event_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Position is one source system's side of a conflict.
type Position struct {
	SourceSystem  string   `json:"source_system"`
	Authoritative bool     `json:"authoritative"`
	Objects       []Object `json:"objects"`
}

// Conflict is a disagreement across source systems about a subject's
// predicate.
type Conflict struct {
	Subject    Ref        `json:"subject"`
	Predicate  string     `json:"predicate"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`
	Positions  []Position `json:"positions"`
	Resolution string     `json:"resolution,omitempty"`
}

// Entity is what Get knows about a subject at a point.
type Entity struct {
	Point     Point      `json:"point"`
	Subject   Ref        `json:"subject"`
	Status    string     `json:"status"`
	MintedAt  time.Time  `json:"minted_at"`
	MintedBy  string     `json:"minted_by_event,omitempty"`
	Keys      []Key      `json:"keys"`
	Facts     []Fact     `json:"facts"`
	Conflicts []Conflict `json:"conflicts"`
	Merges    []Merge    `json:"merges"`
	Unmerges  []Unmerge  `json:"unmerges,omitempty"`
}

// Get returns everything known about the subject ref names: its keys, its
// facts of every status but none, any conflicts on it and its merges. The
// facts are the ones whose subject it is; Related lists the ones that name
// it as their object.
func (q *Querier) Get(ctx context.Context, ref string, p Point) (*Entity, error) {
	id, err := q.Resolve(ctx, ref, p)
	if err != nil {
		return nil, err
	}
	l := q.labeler(p)
	s, err := q.Graph.Subject(ctx, id, p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("subject %s: %w", id, err)
	}
	subject, err := l.ref(ctx, string(id))
	if err != nil {
		return nil, err
	}
	e := &Entity{
		Point: p, Subject: subject, Status: enumName("SUBJECT_STATUS_", s.GetStatus().String()),
		MintedAt: s.GetMintedAt().AsTime(), MintedBy: s.GetMintedBy().GetEventId(),
	}
	if e.Keys, err = q.keys(ctx, l, id); err != nil {
		return nil, err
	}
	states, err := q.Graph.AsOf(ctx, contracts.FactFilter{SubjectID: id}, p.Valid, p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("facts of %s: %w", id, err)
	}
	if e.Facts, err = l.facts(ctx, states); err != nil {
		return nil, err
	}
	if e.Conflicts, err = q.conflicts(ctx, l, id, ""); err != nil {
		return nil, err
	}
	if e.Merges, err = q.merges(ctx, l, id); err != nil {
		return nil, err
	}
	if e.Unmerges, err = q.unmerges(ctx, l, id); err != nil {
		return nil, err
	}
	return e, nil
}

// keys lists the aliases that lead to the subject at the point, sorted.
func (q *Querier) keys(ctx context.Context, l *labeler, id contracts.SubjectID) ([]Key, error) {
	rows, err := q.Graph.Bindings(ctx, nil, []contracts.SubjectID{id}, l.p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("keys of %s: %w", id, err)
	}
	keys := []Key{}
	for _, b := range rows {
		if b.GetSubjectId() == "" || !covers(b, l.p.Valid) {
			continue
		}
		canon, err := q.Resolve(ctx, b.GetSubjectId(), Point{Recorded: l.p.Recorded})
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if err != nil || canon != id {
			continue // bound to another subject
		}
		keys = append(keys, Key{Alias: b.GetAlias(), Redirect: b.GetReleased(), ValidFrom: optTime(b.GetValidFrom()), ValidTo: optTime(b.GetValidTo())})
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if a.Redirect != b.Redirect {
			if a.Redirect {
				return 1
			}
			return -1
		}
		return compareStrings(a.Alias, b.Alias)
	})
	return keys, nil
}

// covers reports whether the binding row holds at valid time v. A zero v is
// now, which the store alone knows: a row still open at its end holds then,
// and one that has ended does not (Bearing writes endings as they happen, so
// no row starts or ends in the future).
func covers(b *modelv1alpha1.Binding, v time.Time) bool {
	if v.IsZero() {
		return b.GetValidTo() == nil
	}
	return (b.GetValidFrom() == nil || !b.GetValidFrom().AsTime().After(v)) &&
		(b.GetValidTo() == nil || b.GetValidTo().AsTime().After(v))
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
