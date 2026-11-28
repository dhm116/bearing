// Package query answers the CLI's and the API's questions from a
// [graph.Graph]: what an entity is, who owns it, what it relates to, what
// changed and what matches a name. Every fact in an answer carries its
// sources (the observations behind it) and its confidence. Owners, Related,
// Changes and Runtimes answer per record, with each claim's path (ADR 11);
// Get, Find and Links answer per cluster.
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
	// MaxFindText bounds the text Find looks for, in bytes: a name is short,
	// and the store scores the text against every entity.
	MaxFindText = 256
	// MaxTeamMembers bounds the people Owners lists per owning team.
	MaxTeamMembers = 50
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
	// HalfLife is how fast a kind's link evidence fades, for the scores
	// Links shows; nil means the matching defaults (match.Config{}).
	HalfLife func(model.Kind) time.Duration
	// Registry is the vocabulary: which kinds exist and which extend which;
	// nil means model.Core().
	Registry *model.Registry
}

// Ref names an entity in an answer: enough to read it and to ask about it
// next. In a claim, a change or a runtime it names one record, by the
// record's own name and key, and carries the record's cluster (ADR 11); in
// Get, Find and team members it names a cluster and Cluster is empty.
type Ref struct {
	ID      string `json:"id"`
	Cluster string `json:"cluster,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Name    string `json:"name,omitempty"`
	// Key is the first key, sorted.
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
	// ID is the stored fact's, which a claim's path names; a fact merged from
	// several keeps the first one's.
	ID        string `json:"id,omitempty"`
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
	// AsOf is when the subject's kind was retired, set on current answers
	// only: the fact is true as of then, and no source has been asked for
	// it since. History answers (Changes) leave it unset.
	AsOf *time.Time `json:"as_of,omitempty"`
	// Sources are oldest first: per source, the first and the latest
	// observation. SourceCount is how many observations there are in all.
	Sources     []Source `json:"sources"`
	SourceCount int      `json:"source_count"`
}

// Entity is the answer to Get: the entity, its keys and the facts it is the
// subject of.
type Entity struct {
	Ref
	Keys  []string `json:"keys"`
	Facts []Fact   `json:"facts"`
}

// Ownership is the answer to Owners: a claim per owner fact the subject
// records reach.
type Ownership struct {
	Answer
	// Teams are the people on each owning team; only when asked for
	// (members), and only for owners that are teams, one per cluster.
	Teams []TeamMembers `json:"teams,omitempty"`
}

// TeamMembers is the people on one owning team at the answer's time.
type TeamMembers struct {
	// Team is the record the first claim names; its members are the
	// cluster's.
	Team    Ref      `json:"team"`
	Members []Member `json:"members"`
	// Truncated is set when the team has more than MaxTeamMembers people.
	Truncated bool `json:"members_truncated,omitempty"`
}

// Member is a person with a current member_of fact to a team.
type Member struct {
	Ref
	// Role is the team role a source gives ("maintainer" or "member" on
	// GitHub); empty when none does.
	Role string `json:"role,omitempty"`
	// Since is when the membership was first observed.
	Since time.Time `json:"since"`
	// AsOf is when the person's kind was retired, as for Fact.AsOf.
	AsOf *time.Time `json:"as_of,omitempty"`
	// Sources are the observations behind the membership, as for a Fact.
	Sources []Source `json:"sources"`
}

// Related is the answer to Related: a claim per fact the walk crossed.
type Related struct {
	Answer
	Direction string `json:"direction"`
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
	l := q.currentLabeler(at)
	out := &Entity{Ref: l.add(e), Keys: make([]string, len(e.Keys))}
	for i, k := range e.Keys {
		out.Keys[i] = string(k)
	}
	if out.Facts, err = l.facts(ctx, merge(facts)); err != nil {
		return nil, err
	}
	return out, nil
}

// Owners answers which team or person owns the entity ref names (ADR 11):
// a claim per asserted owned_by fact the subject records reach at at or,
// when they reach none, per approves_changes fact (CODEOWNERS). Each claim
// keeps its predicate, so a caller sees which rule answered. With members it
// also lists the people on each owning team (current members at at,
// maintainers first); without, it asks the store nothing more.
func (q *Querier) Owners(ctx context.Context, ref string, at time.Time, members bool) (*Ownership, error) {
	subs, hit, err := q.subjects(ctx, ref)
	if err != nil {
		return nil, err
	}
	a, err := q.answer(ctx, subs, ownerAsk, at)
	if err != nil {
		return nil, err
	}
	a.LimitHit = hit
	out := &Ownership{Answer: a}
	if members {
		if out.Teams, err = q.teams(ctx, q.currentLabeler(at), a.Claims, at); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// teams lists the people on each owner cluster that is not a person.
func (q *Querier) teams(ctx context.Context, l *labeler, owners []Claim, at time.Time) ([]TeamMembers, error) {
	var out []TeamMembers
	reg := q.Registry
	person := func(kind string) bool { return reg.IsA(model.Kind(kind), model.KindPerson) }
	seen := map[string]bool{}
	for _, c := range owners {
		o := c.Fact.Object
		if o == nil || person(o.Kind) || seen[o.Cluster] {
			continue
		}
		seen[o.Cluster] = true
		facts, err := q.Graph.Facts(ctx, graph.FactFilter{Predicate: "member_of", Object: graph.EntityID(o.Cluster), At: at})
		if err != nil {
			return nil, fmt.Errorf("members of %s: %w", o.Cluster, err)
		}
		// Teams in teams are not people; skip them.
		people := []Member{}
		for _, f := range merge(facts) {
			r, err := l.ref(ctx, f.Subject)
			if err != nil {
				return nil, err
			}
			if !person(r.Kind) {
				continue
			}
			vf, err := l.fact(ctx, f)
			if err != nil {
				return nil, err
			}
			people = append(people, Member{Ref: r, Role: roleOf(facts, f.Subject), Since: vf.ValidFrom, AsOf: vf.AsOf, Sources: vf.Sources})
		}
		slices.SortFunc(people, func(a, b Member) int {
			return cmp.Or(cmp.Compare(rank(a.Role), rank(b.Role)), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
		})
		tm := TeamMembers{Team: *o, Members: people}
		if len(people) > MaxTeamMembers {
			tm.Members, tm.Truncated = people[:MaxTeamMembers], true
		}
		out = append(out, tm)
	}
	return out, nil
}

// roleOf is the role the most recently observed source gives person, or
// empty: a source with no roles (the directory) does not hide another's.
func roleOf(facts []graph.Fact, person graph.EntityID) string {
	var role string
	var seen time.Time
	for _, f := range facts {
		r, _ := f.Qualifiers["role"].(string)
		if f.Subject == person && r != "" && !lastObserved(f).Before(seen) {
			role, seen = r, lastObserved(f)
		}
	}
	return role
}

// rank orders roles: maintainers, other roles, then people with none.
func rank(role string) int {
	switch role {
	case "maintainer":
		return 0
	case "":
		return 2
	}
	return 1
}

// Related walks relations from the records ref names (ADR 11): predicate
// alone, or every relation when empty; direction "out" (subject to object,
// the default) or "in"; up to depth facts (zero means 1, at most MaxDepth),
// at at. Each fact crossed is one claim, with its shortest path.
func (q *Querier) Related(ctx context.Context, ref, predicate, direction string, depth int, at time.Time) (*Related, error) {
	a := ask{preds: [][]string{nil}, depth: max(depth, 1)}
	switch direction {
	case "", "out":
		direction = "out"
	case "in":
		a.reverse = true
	default:
		return nil, fmt.Errorf("%w: direction must be out or in", ErrInvalidRequest)
	}
	if depth < 0 || depth > MaxDepth {
		return nil, fmt.Errorf("%w: depth must be from 1 to %d", ErrInvalidRequest, MaxDepth)
	}
	if predicate != "" {
		a.preds = [][]string{{predicate}}
	}
	subs, hit, err := q.subjects(ctx, ref)
	if err != nil {
		return nil, err
	}
	ans, err := q.answer(ctx, subs, a, at)
	if err != nil {
		return nil, err
	}
	ans.LimitHit = hit
	return &Related{Answer: ans, Direction: direction}, nil
}

// Find returns the entities whose name or keys resemble text, best first.
// An empty kind matches every kind, and a kind its descendants; limit zero
// asks for the store's default.
func (q *Querier) Find(ctx context.Context, text, kind string, limit int) ([]Match, error) {
	switch {
	case text == "":
		return nil, fmt.Errorf("%w: a query is required", ErrInvalidRequest)
	case len(text) > MaxFindText:
		return nil, fmt.Errorf("%w: the query is longer than %d bytes", ErrInvalidRequest, MaxFindText)
	case limit < 0 || limit > MaxFindLimit:
		return nil, fmt.Errorf("%w: limit must be from 1 to %d", ErrInvalidRequest, MaxFindLimit)
	case kind != "" && !q.Registry.Valid(model.Kind(kind)):
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
	// current is set for answers at a time (Get, Owners, Related): facts of
	// a kind retired by then get AsOf.
	current bool
	at      time.Time
}

// currentLabeler is a labeler for answers at at (zero is now).
func (q *Querier) currentLabeler(at time.Time) *labeler {
	l := q.labeler()
	l.current, l.at = true, at
	return l
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
	var obj *Ref
	if f.Object != "" {
		o, err := l.ref(ctx, f.Object)
		if err != nil {
			return Fact{}, err
		}
		obj = &o
	}
	return l.build(f, sub, obj), nil
}

// build is f in an answer, named by sub and obj.
func (l *labeler) build(f graph.Fact, sub Ref, obj *Ref) Fact {
	out := Fact{
		ID: f.ID, Subject: sub, Predicate: f.Predicate, Object: obj, Value: f.Value, Qualifiers: f.Qualifiers, Status: string(f.Status),
		Confidence: f.Confidence, ValidFrom: f.ValidFrom, SourceCount: f.SourceCount, Sources: make([]Source, len(f.Sources)),
	}
	if !f.ValidTo.IsZero() {
		out.ValidTo = &f.ValidTo
	}
	for i, s := range f.Sources {
		out.Sources[i] = Source{Source: s.Source, ObservationID: s.ObservationID, URL: s.URL, ObservedAt: s.ObservedAt}
	}
	if t, ok := l.q.Registry.Retired(model.Kind(sub.Kind)); ok && l.current && (l.at.IsZero() || !l.at.Before(t)) {
		out.AsOf = &t
	}
	return out
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
// and every source, oldest first, their counts summed. Statements keep the
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
			cur.SourceCount += f.SourceCount
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
