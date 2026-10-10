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
	// ChangedAt is when the fact took the status and confidence it has at the
	// window's end (docs/spec/data-model.md, "What changed").
	ChangedAt time.Time `json:"changed_at"`
	// SupportsChanged are the sources whose supports differ between the ends.
	SupportsChanged []string `json:"supports_changed"`
	// Before are the supports the fact had at the window's start, After at
	// its end. A fact that came into being has none before; one that ended
	// has none after.
	Before []Support `json:"before"`
	After  []Support `json:"after"`
}

// Changes are one page of the facts that differ across a window, newest
// first.
type Changes struct {
	Axis Axis `json:"axis"`
	// Since and Until bound the window, as resolved: a question that named no
	// end is answered up to the time it was asked.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// DefaultWindow is set when the question named neither end, so the
	// window is the last [DefaultChangesWindow].
	DefaultWindow bool     `json:"default_window,omitempty"`
	Subject       *Ref     `json:"subject,omitempty"`
	Changes       []Change `json:"changes"`
	// NextPageToken asks for the next page when more changes follow. It is
	// opaque to callers and carries the whole question.
	NextPageToken string `json:"next_page_token,omitempty"`
	// MostRecentChange is, on a first page with no changes, when the newest
	// change before the window happened. It is empty when none ever did.
	MostRecentChange *time.Time `json:"most_recent_change,omitempty"`
}

// DefaultChangesWindow is how far back a question that names no start looks.
const DefaultChangesWindow = 24 * time.Hour

// ChangesRequest asks Querier.Changes for a page of changes.
type ChangesRequest struct {
	// Ref limits the answer to facts about a subject: its ID or a key. Empty
	// asks about the whole graph.
	Ref string
	// Since and Until bound the window (Since, Until]. A zero Until is now,
	// and a zero Since is [DefaultChangesWindow] before Until.
	Since, Until time.Time
	// Axis defaults to [AxisValid].
	Axis Axis
	// Limit is the page size: [contracts.DefaultChangesLimit] when zero, at
	// most [contracts.MaxChangesLimit].
	Limit int
	// PageToken continues an earlier answer: a NextPageToken. It carries the
	// question, so Ref, Since, Until and Axis must be empty.
	PageToken string
}

// ErrBadPageToken is returned for a page token that Changes did not issue
// or that cannot be read.
var ErrBadPageToken = errors.New("query: invalid page token")

// Changes returns a page of the facts whose status or confidence differs
// between the ends of a window, newest first. It is an endpoint diff: steps
// in between are not listed. A question that names no window gets the last
// [DefaultChangesWindow]; when nothing changed in a window, the answer says
// when the newest change before it was. A page that is not the last carries
// the token for the next.
func (q *Querier) Changes(ctx context.Context, req ChangesRequest) (*Changes, error) {
	w, err := q.changesWindow(req)
	if err != nil {
		return nil, err
	}
	var a contracts.Axis
	switch w.axis {
	case AxisValid:
		a = contracts.AxisValid
	case AxisRecord:
		a = contracts.AxisRecord
	default:
		return nil, fmt.Errorf("unknown axis %q: want %s or %s", w.axis, AxisValid, AxisRecord)
	}
	if req.Limit < 0 || req.Limit > contracts.MaxChangesLimit {
		return nil, fmt.Errorf("limit %d: want 1 to %d", req.Limit, contracts.MaxChangesLimit)
	}
	if w.since.After(w.until) {
		return nil, errors.New("the window starts after it ends")
	}
	end := pointAt(w.until, w.axis)
	l := q.labeler(end)
	out := &Changes{Axis: w.axis, Since: w.since, Until: w.until, DefaultWindow: w.deflt, Changes: []Change{}}
	var filter contracts.FactFilter
	switch {
	case w.subject != "":
		filter.SubjectID = contracts.SubjectID(w.subject)
	case req.Ref != "":
		// The subject is looked up as the world stands at the window's end.
		id, err := q.Resolve(ctx, req.Ref, end)
		if err != nil {
			return nil, err
		}
		filter.SubjectID, w.subject = id, string(id)
	}
	if filter.SubjectID != "" {
		r, err := l.ref(ctx, string(filter.SubjectID))
		if err != nil {
			return nil, err
		}
		out.Subject = &r
	}
	page, err := q.Graph.ChangesPage(ctx, contracts.ChangesRequest{
		Filter: filter, T1: w.since, T2: w.until, Axis: a, Limit: req.Limit, After: w.after,
	})
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	for _, c := range page.Changes {
		ch, err := q.change(ctx, l, c, w.since, w.until, w.axis)
		if err != nil {
			return nil, err
		}
		out.Changes = append(out.Changes, ch)
	}
	switch {
	case page.Next != nil:
		w.after = page.Next
		out.NextPageToken = w.encode()
	case len(page.Changes) == 0 && req.PageToken == "":
		// Nothing in the window: say when anything last changed, so the
		// answer is not just "none".
		at, err := q.Graph.LastChange(ctx, filter, w.since, a)
		switch {
		case errors.Is(err, contracts.ErrNotFound):
		case err != nil:
			return nil, fmt.Errorf("last change: %w", err)
		default:
			out.MostRecentChange = &at
		}
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
		FactID: c.GetFactId(), Subject: subject, Predicate: c.GetPredicate(), Object: object, ChangedAt: c.GetChangedAt().AsTime(),
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
