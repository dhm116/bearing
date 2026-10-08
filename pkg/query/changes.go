package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// Axis picks what Changes compares (docs/spec/data-model.md, "What changed").
type Axis string

// The axes.
const (
	// AxisValid compares the world at two valid times, as known now.
	AxisValid Axis = "valid"
	// AxisRecord compares Bearing's answers as recorded at two times.
	AxisRecord Axis = "record"
)

// StatusPoint is a fact's status and confidence at one end of a change.
type StatusPoint struct {
	Status        string `json:"status"`
	ConfidencePPM uint32 `json:"confidence_ppm"`
}

// Change is a fact whose status or confidence differs between the two ends
// of a window.
type Change struct {
	FactID    string      `json:"fact_id"`
	Subject   Ref         `json:"subject"`
	Predicate string      `json:"predicate"`
	Object    Object      `json:"object"`
	From      StatusPoint `json:"from"`
	To        StatusPoint `json:"to"`
	// SupportsChanged are the sources whose supports differ between the ends.
	SupportsChanged []string `json:"supports_changed"`
	// Before are the supports the fact had at the window's start, After at
	// its end. A fact that came into being has none before; one that ended
	// has none after.
	Before []Support `json:"before"`
	After  []Support `json:"after"`
}

// Changes are the facts that differ across a window.
type Changes struct {
	Axis Axis `json:"axis"`
	// Since and Until bound the window; a zero Until is now.
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until,omitzero"`
	Subject *Ref      `json:"subject,omitempty"`
	Changes []Change  `json:"changes"`
}

// Changes returns the facts whose status or confidence differs between since
// and until (zero is now). It is an endpoint diff: steps in between are not
// listed. A non-empty ref limits it to facts about that subject.
func (q *Querier) Changes(ctx context.Context, ref string, since, until time.Time, axis Axis) (*Changes, error) {
	var a contracts.Axis
	switch axis {
	case AxisValid:
		a = contracts.AxisValid
	case AxisRecord:
		a = contracts.AxisRecord
	default:
		return nil, fmt.Errorf("unknown axis %q: want %s or %s", axis, AxisValid, AxisRecord)
	}
	if !until.IsZero() && since.After(until) {
		return nil, errors.New("--since is after the end of the window")
	}
	l := q.labeler(Point{Valid: until, Recorded: until})
	out := &Changes{Axis: axis, Since: since, Until: until, Changes: []Change{}}
	var filter contracts.FactFilter
	if ref != "" {
		// The subject is looked up as the world stands at the window's end.
		id, err := q.Resolve(ctx, ref, Point{Valid: until})
		if err != nil {
			return nil, err
		}
		filter.SubjectID = id
		r, err := l.ref(ctx, string(id))
		if err != nil {
			return nil, err
		}
		out.Subject = &r
	}
	in, err := q.Graph.Changes(ctx, filter, since, until, a)
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	for _, c := range in {
		ch, err := q.change(ctx, l, c, since, until, axis)
		if err != nil {
			return nil, err
		}
		out.Changes = append(out.Changes, ch)
	}
	return out, nil
}

func (q *Querier) change(ctx context.Context, l *labeler, c *modelv1alpha1.FactChange, since, until time.Time, axis Axis) (Change, error) {
	subject, err := l.ref(ctx, c.GetSubjectId())
	if err != nil {
		return Change{}, err
	}
	object, err := l.object(ctx, c.GetObject())
	if err != nil {
		return Change{}, err
	}
	ch := Change{
		FactID: c.GetFactId(), Subject: subject, Predicate: c.GetPredicate(), Object: object,
		From: point(c.GetFrom()), To: point(c.GetTo()), SupportsChanged: c.GetSupportsChanged(),
	}
	if ch.SupportsChanged == nil {
		ch.SupportsChanged = []string{}
	}
	if ch.Before, err = q.supportsOf(ctx, c, pointAt(since, axis)); err != nil {
		return Change{}, err
	}
	if ch.After, err = q.supportsOf(ctx, c, pointAt(until, axis)); err != nil {
		return Change{}, err
	}
	return ch, nil
}

func point(p *modelv1alpha1.FactPoint) StatusPoint {
	return StatusPoint{Status: enumName("FACT_STATUS_", p.GetStatus().String()), ConfidencePPM: p.GetConfidencePpm()}
}

// pointAt is where one end of a window is read: the valid axis varies valid
// time and reads what is known now, the record axis varies both.
func pointAt(t time.Time, axis Axis) Point {
	if axis == AxisRecord {
		return Point{Valid: t, Recorded: t}
	}
	return Point{Valid: t}
}

// supportsOf returns the live supports of the changed fact at p, if it has
// any there.
func (q *Querier) supportsOf(ctx context.Context, c *modelv1alpha1.FactChange, p Point) ([]Support, error) {
	states, err := q.Graph.AsOf(ctx, contracts.FactFilter{
		SubjectID: contracts.SubjectID(c.GetSubjectId()), Predicate: c.GetPredicate(), Object: c.GetObject(),
	}, p.Valid, p.Recorded)
	if err != nil {
		return nil, fmt.Errorf("supports of fact %s: %w", c.GetFactId(), err)
	}
	for _, s := range states {
		if s.GetFactId() == c.GetFactId() {
			return supports(s.GetSupports()), nil
		}
	}
	return []Support{}, nil
}
