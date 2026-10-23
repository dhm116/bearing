// Package query answers the CLI's and the API's questions from a
// [graph.Graph]: what an entity is, who owns it, what it relates to, what
// changed and what matches a name. Every fact in an answer carries its
// sources (the observations behind it) and its confidence.
//
// It is the layer between the commands and the store; its types are the
// API's JSON (docs/spec/api.md), so nothing here prints or reads flags.
package query

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// ErrNotFound is returned when a reference names nothing. It wraps
// graph.ErrNotFound.
var ErrNotFound = fmt.Errorf("query: %w", graph.ErrNotFound)

// ErrInvalidRequest is wrapped by the errors that say a question is malformed
// (no reference, a direction or limit out of range, a window that ends before
// it starts), as opposed to one the store could not answer.
var ErrInvalidRequest = errors.New("query: invalid request")

// Limits of a question.
const (
	// MaxDepth bounds Related.
	MaxDepth = 10
	// MaxChangesLimit bounds Changes; zero asks for the store's default.
	MaxChangesLimit = 1000
	// MaxFindLimit bounds Find; zero asks for the store's default.
	MaxFindLimit = 100
	// defaultWindow is how far back Changes looks when no start is given.
	defaultWindow = 24 * time.Hour
)

// Owner predicates: owned_by answers first; approves_changes (CODEOWNERS)
// answers when an entity has no owned_by fact.
const (
	predOwnedBy         = "owned_by"
	predApprovesChanges = "approves_changes"
)

// Querier reads one graph store.
type Querier struct {
	Graph graph.Graph
	// Now is the time Changes counts its default window back from; nil means
	// time.Now.
	Now func() time.Time
}

// Ref names an entity in an answer: enough to read it and to ask about it
// next.
type Ref struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	// Key is the entity's first key, sorted.
	Key string `json:"key,omitempty"`
}

// Source is one observation behind a fact.
type Source struct {
	Source        string    `json:"source"`
	ObservationID string    `json:"observation_id"`
	URL           string    `json:"url,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// Fact is one statement: a relation to Object or an attribute Value. Facts
// the store keeps per source are answered as one when they say the same
// thing over overlapping time.
type Fact struct {
	Subject   Ref    `json:"subject"`
	Predicate string `json:"predicate"`
	Object    *Ref   `json:"object,omitempty"`
	Value     any    `json:"value,omitempty"`
	// Qualifiers are a relation's details; of several sources, the most
	// recently observed one's.
	Qualifiers map[string]any `json:"qualifiers,omitempty"`
	Status     string         `json:"status"`
	// Confidence is between 0 and 1.
	Confidence float64    `json:"confidence"`
	ValidFrom  time.Time  `json:"valid_from"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`
	// Sources are oldest first.
	Sources []Source `json:"sources"`
}

// Entity is the answer to Get: the entity, its keys and the facts it is the
// subject of.
type Entity struct {
	Ref
	Keys  []string `json:"keys"`
	Facts []Fact   `json:"facts"`
}

// Ownership is the answer to Owners.
type Ownership struct {
	Subject Ref    `json:"subject"`
	Owners  []Fact `json:"owners"`
}

// Relations is the answer to Related.
type Relations struct {
	Subject   Ref    `json:"subject"`
	Direction string `json:"direction"`
	Steps     []Step `json:"steps"`
}

// Step is a fact Related crossed, Depth hops from the subject.
type Step struct {
	Depth int  `json:"depth"`
	Fact  Fact `json:"fact"`
}

// Change is a fact that was opened or closed At.
type Change struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Fact Fact      `json:"fact"`
}

// Changes is the answer to Changes, newest first.
type Changes struct {
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Changes []Change  `json:"changes"`
}

// Match is one Find result.
type Match struct {
	Ref
	// Score is between 0 and 1; higher is closer.
	Score float64 `json:"score"`
}

// Get returns the entity ref names and the facts about it at at (zero is
// now).
func (q *Querier) Get(ctx context.Context, ref string, at time.Time) (*Entity, error) {
	e, err := q.entity(ctx, ref)
	if err != nil {
		return nil, err
	}
	facts, err := q.Graph.Facts(ctx, graph.FactFilter{Subject: e.ID, At: at})
	if err != nil {
		return nil, fmt.Errorf("facts of %s: %w", e.ID, err)
	}
	l := q.labeler()
	out := &Entity{Ref: l.add(e), Keys: make([]string, len(e.Keys))}
	for i, k := range e.Keys {
		out.Keys[i] = string(k)
	}
	if out.Facts, err = l.facts(ctx, merge(facts)); err != nil {
		return nil, err
	}
	return out, nil
}

// Owners returns the asserted owned_by facts of the entity at at or, when it
// has none, its asserted approves_changes facts (CODEOWNERS). Each fact keeps
// its predicate, so a caller sees which rule answered.
func (q *Querier) Owners(ctx context.Context, ref string, at time.Time) (*Ownership, error) {
	e, err := q.entity(ctx, ref)
	if err != nil {
		return nil, err
	}
	var facts []graph.Fact
	for _, p := range []string{predOwnedBy, predApprovesChanges} {
		if facts, err = q.Graph.Facts(ctx, graph.FactFilter{Subject: e.ID, Predicate: p, At: at}); err != nil {
			return nil, fmt.Errorf("%s of %s: %w", p, e.ID, err)
		}
		if len(facts) > 0 {
			break
		}
	}
	l := q.labeler()
	out := &Ownership{Subject: l.add(e)}
	if out.Owners, err = l.facts(ctx, merge(facts)); err != nil {
		return nil, err
	}
	return out, nil
}

// Related walks relations from the entity ref names: predicate alone, or every
// relation when empty; direction "out" (subject to object, the default) or
// "in"; up to depth hops (zero means 1, at most MaxDepth), at at.
func (q *Querier) Related(ctx context.Context, ref, predicate, direction string, depth int, at time.Time) (*Relations, error) {
	dir := graph.Out
	switch direction {
	case "", "out":
		direction = "out"
	case "in":
		dir = graph.In
	default:
		return nil, fmt.Errorf("%w: direction must be out or in", ErrInvalidRequest)
	}
	if depth < 0 || depth > MaxDepth {
		return nil, fmt.Errorf("%w: depth must be from 1 to %d", ErrInvalidRequest, MaxDepth)
	}
	e, err := q.entity(ctx, ref)
	if err != nil {
		return nil, err
	}
	req := graph.WalkRequest{From: e.ID, Direction: dir, MaxDepth: max(depth, 1), At: at}
	if predicate != "" {
		req.Predicates = []string{predicate}
	}
	steps, err := q.Graph.Walk(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("walk from %s: %w", e.ID, err)
	}
	l := q.labeler()
	out := &Relations{Subject: l.add(e), Direction: direction, Steps: []Step{}}
	// One fact per statement at each depth.
	for d, group := range groupBy(steps, func(s graph.Step) int { return s.Depth }) {
		facts := make([]graph.Fact, len(group))
		for i, s := range group {
			facts[i] = s.Fact
		}
		fs, err := l.facts(ctx, merge(facts))
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			out.Steps = append(out.Steps, Step{Depth: d, Fact: f})
		}
	}
	return out, nil
}

// Changes returns the facts opened or closed in [since, until), about the
// entity ref names or, when ref is empty, about anything. until defaults to
// now and since to 24 hours before until; limit zero asks for the store's
// default.
func (q *Querier) Changes(ctx context.Context, ref string, since, until time.Time, limit int) (*Changes, error) {
	if limit < 0 || limit > MaxChangesLimit {
		return nil, fmt.Errorf("%w: limit must be from 1 to %d", ErrInvalidRequest, MaxChangesLimit)
	}
	if until.IsZero() {
		now := time.Now
		if q.Now != nil {
			now = q.Now
		}
		until = now().UTC()
	}
	if since.IsZero() {
		since = until.Add(-defaultWindow)
	}
	if !since.Before(until) {
		return nil, fmt.Errorf("%w: the window starts at or after its end", ErrInvalidRequest)
	}
	req := graph.ChangesRequest{Since: since, Until: until, Limit: limit}
	if ref != "" {
		e, err := q.entity(ctx, ref)
		if err != nil {
			return nil, err
		}
		req.Subject = e.ID
	}
	changes, err := q.Graph.Changes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	l := q.labeler()
	out := &Changes{Since: since, Until: until, Changes: make([]Change, 0, len(changes))}
	for _, c := range changes {
		f, err := l.fact(ctx, c.Fact)
		if err != nil {
			return nil, err
		}
		out.Changes = append(out.Changes, Change{Kind: string(c.Kind), At: c.At, Fact: f})
	}
	return out, nil
}

// Find returns the entities whose name or keys resemble text, best first.
// An empty kind matches every kind; limit zero asks for the store's default.
func (q *Querier) Find(ctx context.Context, text, kind string, limit int) ([]Match, error) {
	switch {
	case text == "":
		return nil, fmt.Errorf("%w: a query is required", ErrInvalidRequest)
	case limit < 0 || limit > MaxFindLimit:
		return nil, fmt.Errorf("%w: limit must be from 1 to %d", ErrInvalidRequest, MaxFindLimit)
	case kind != "" && !model.Kind(kind).Valid():
		return nil, fmt.Errorf("%w: kind %q is not a kind", ErrInvalidRequest, model.Clip(kind))
	}
	ms, err := q.Graph.Find(ctx, text, model.Kind(kind), limit)
	if err != nil {
		return nil, fmt.Errorf("find: %w", err)
	}
	l := q.labeler()
	out := make([]Match, len(ms))
	for i, m := range ms {
		out[i] = Match{Ref: l.add(m.Entity), Score: m.Score}
	}
	return out, nil
}

// entity looks up a reference: an entity ID or a key.
func (q *Querier) entity(ctx context.Context, ref string) (graph.Entity, error) {
	if ref == "" {
		return graph.Entity{}, fmt.Errorf("%w: a reference is required", ErrInvalidRequest)
	}
	e, err := q.Graph.Entity(ctx, ref)
	if errors.Is(err, graph.ErrNotFound) {
		return graph.Entity{}, fmt.Errorf("entity %s: %w", ref, ErrNotFound)
	}
	if err != nil {
		return graph.Entity{}, fmt.Errorf("entity %s: %w", ref, err)
	}
	return e, nil
}

// labeler builds Refs, caching them for one question.
type labeler struct {
	q     *Querier
	cache map[graph.EntityID]Ref
}

func (q *Querier) labeler() *labeler {
	return &labeler{q: q, cache: map[graph.EntityID]Ref{}}
}

func (l *labeler) add(e graph.Entity) Ref {
	r := Ref{ID: string(e.ID), Kind: string(e.Kind), Name: e.Name}
	if len(e.Keys) > 0 {
		r.Key = string(e.Keys[0])
	}
	l.cache[e.ID] = r
	return r
}

func (l *labeler) ref(ctx context.Context, id graph.EntityID) (Ref, error) {
	if r, ok := l.cache[id]; ok {
		return r, nil
	}
	e, err := l.q.Graph.Entity(ctx, string(id))
	if errors.Is(err, graph.ErrNotFound) {
		// Facts name survivors, so this is unexpected; the ID is still usable.
		l.cache[id] = Ref{ID: string(id)}
		return l.cache[id], nil
	}
	if err != nil {
		return Ref{}, fmt.Errorf("entity %s: %w", id, err)
	}
	return l.add(e), nil
}

func (l *labeler) fact(ctx context.Context, f graph.Fact) (Fact, error) {
	sub, err := l.ref(ctx, f.Subject)
	if err != nil {
		return Fact{}, err
	}
	out := Fact{
		Subject: sub, Predicate: f.Predicate, Value: f.Value, Qualifiers: f.Qualifiers, Status: string(f.Status),
		Confidence: f.Confidence, ValidFrom: f.ValidFrom, Sources: make([]Source, len(f.Sources)),
	}
	if f.Object != "" {
		obj, err := l.ref(ctx, f.Object)
		if err != nil {
			return Fact{}, err
		}
		out.Object = &obj
	}
	if !f.ValidTo.IsZero() {
		out.ValidTo = &f.ValidTo
	}
	for i, s := range f.Sources {
		out.Sources[i] = Source{Source: s.Source, ObservationID: s.ObservationID, URL: s.URL, ObservedAt: s.ObservedAt}
	}
	return out, nil
}

func (l *labeler) facts(ctx context.Context, fs []graph.Fact) ([]Fact, error) {
	out := make([]Fact, 0, len(fs))
	for _, f := range fs {
		v, err := l.fact(ctx, f)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// merge answers one fact per statement: facts with the same subject,
// predicate, object or value and status whose validity overlaps become one,
// with the earliest ValidFrom, the latest ValidTo (open if any is), the
// highest confidence, the qualifiers of the most recently observed source
// and every source, oldest first. Statements keep the
// order of their first fact.
func merge(facts []graph.Fact) []graph.Fact {
	var out []graph.Fact
	for _, group := range groupBy(facts, statement) {
		slices.SortStableFunc(group, func(a, b graph.Fact) int { return a.ValidFrom.Compare(b.ValidFrom) })
		cur := group[0]
		cur.Sources = slices.Clone(cur.Sources)
		for _, f := range group[1:] {
			if !cur.ValidTo.IsZero() && f.ValidFrom.After(cur.ValidTo) {
				out = append(out, finish(cur))
				cur = f
				cur.Sources = slices.Clone(cur.Sources)
				continue
			}
			if cur.ValidTo.IsZero() || f.ValidTo.IsZero() {
				cur.ValidTo = time.Time{}
			} else if f.ValidTo.After(cur.ValidTo) {
				cur.ValidTo = f.ValidTo
			}
			if lastObserved(f).After(lastObserved(cur)) {
				cur.Qualifiers = f.Qualifiers
			}
			cur.Confidence = max(cur.Confidence, f.Confidence)
			cur.Sources = append(cur.Sources, f.Sources...)
		}
		out = append(out, finish(cur))
	}
	return out
}

// lastObserved is when the newest of the fact's sources was observed.
func lastObserved(f graph.Fact) time.Time {
	var t time.Time
	for _, s := range f.Sources {
		if s.ObservedAt.After(t) {
			t = s.ObservedAt
		}
	}
	return t
}

// finish sorts a merged fact's sources oldest first, each observation once.
func finish(f graph.Fact) graph.Fact {
	slices.SortStableFunc(f.Sources, func(a, b graph.Source) int {
		return cmp.Or(a.ObservedAt.Compare(b.ObservedAt), cmp.Compare(a.ObservationID, b.ObservationID))
	})
	f.Sources = slices.CompactFunc(f.Sources, func(a, b graph.Source) bool { return a.ObservationID == b.ObservationID })
	return f
}

// statement is what makes two facts say the same thing.
func statement(f graph.Fact) string {
	v, _ := json.Marshal(f.Value) // decoded from JSON, so it encodes
	return string(f.Subject) + "\x00" + f.Predicate + "\x00" + string(f.Object) + "\x00" + string(v) + "\x00" + string(f.Status)
}

// groupBy splits xs by key and yields each key with its members, in the
// order of each key's first appearance.
func groupBy[K comparable, T any](xs []T, key func(T) K) func(yield func(K, []T) bool) {
	return func(yield func(K, []T) bool) {
		var keys []K
		groups := map[K][]T{}
		for _, x := range xs {
			k := key(x)
			if _, ok := groups[k]; !ok {
				keys = append(keys, k)
			}
			groups[k] = append(groups[k], x)
		}
		for _, k := range keys {
			if !yield(k, groups[k]) {
				return
			}
		}
	}
}
