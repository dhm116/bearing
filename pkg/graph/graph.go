// Package graph defines the store Bearing keeps entities and facts in
// (ADR 6): the one interface a store implements, and the types its reads
// return. internal/duckstore is the implementation; anything Bearing needs
// from a store is a method here, so another engine can replace it.
package graph

import (
	"context"
	"errors"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/model"
)

// ErrNotFound means nothing matched.
var ErrNotFound = errors.New("not found")

// EntityID identifies one entity. It never changes, but an entity merged
// into another stops being returned; its keys answer for the survivor.
type EntityID string

// Status says whether a fact may answer a question.
type Status string

const (
	// Asserted facts come from deterministic rules or a source's own
	// records, and answer ownership and dependency questions.
	Asserted Status = "asserted"
	// Proposed facts are inferred. They carry evidence for review and never
	// answer those questions.
	Proposed Status = "proposed"
)

// Store keeps observations, entities and facts. Implementations are safe for
// concurrent use.
type Store interface {
	// Ingest validates nothing (callers validate with pkg/model first) and
	// writes obs from one source in one transaction: each observation once
	// by ID, the entities and keys it names, and the facts it opens, refreshes
	// or closes. A failed Ingest writes nothing.
	Ingest(ctx context.Context, source string, obs []*eventv1alpha1.Observation) (IngestResult, error)

	// Entity returns the entity an entity ID or a key names, following
	// merges. It fails with ErrNotFound when nothing matches.
	Entity(ctx context.Context, ref string) (Entity, error)
	// Find returns entities whose name or keys resemble query, best first.
	// An empty kind matches every kind.
	Find(ctx context.Context, query string, kind model.Kind, limit int) ([]Match, error)
	// Facts returns the facts matching f, ordered by subject, predicate and
	// valid_from.
	Facts(ctx context.Context, f FactFilter) ([]Fact, error)
	// Walk follows facts with the given predicates from an entity, breadth
	// first, and returns each fact it crossed with its depth. It visits an
	// entity once, so cycles end.
	Walk(ctx context.Context, r WalkRequest) ([]Step, error)
	// Changes returns the facts opened or closed in [Since, Until), newest
	// first.
	Changes(ctx context.Context, r ChangesRequest) ([]Change, error)

	// Close releases the store.
	Close() error
}

// IngestResult counts what one Ingest wrote.
type IngestResult struct {
	// Observations is how many were new; Duplicates how many were already
	// stored and skipped.
	Observations, Duplicates int
	// Entities is how many entities were created, Merges how many were merged
	// into another.
	Entities, Merges int
	// Opened, Refreshed and Closed count facts.
	Opened, Refreshed, Closed int
}

// Entity is one thing: a service, a repository, a person, a team.
type Entity struct {
	ID   EntityID
	Kind model.Kind
	// Name is for people to read; it is not unique.
	Name string
	// Keys are every key that names the entity, sorted.
	Keys []model.Key
}

// Match is one Find result.
type Match struct {
	Entity Entity
	// Score is between 0 and 1; higher is closer.
	Score float64
}

// Fact is one statement about an entity over a span of time: a relation to
// another entity (Object) or an attribute (Value).
type Fact struct {
	ID        string
	Subject   EntityID
	Predicate string
	// Object is set for a relation and empty for an attribute.
	Object EntityID
	// Value is the attribute's JSON value; nil for a relation.
	Value  any
	Status Status
	// Confidence is between 0 and 1.
	Confidence float64
	// ValidFrom is when a source first observed the fact; ValidTo when it was
	// observed absent, or zero while it holds.
	ValidFrom, ValidTo time.Time
	// Sources are the observations behind the fact, oldest first.
	Sources []Source
}

// Source is one observation behind a fact: the provenance an answer shows.
type Source struct {
	ObservationID string
	// Source is the configured source that delivered it, such as "github-acme".
	Source     string
	ObservedAt time.Time
	// URL links to the record in the source system, when the adapter gave one.
	URL string
}

// FactFilter selects facts. Empty fields match anything.
type FactFilter struct {
	Subject   EntityID
	Predicate string
	Object    EntityID
	// At selects the facts that held then; zero means the facts that hold
	// now.
	At time.Time
	// WithProposed includes proposed facts; by default only asserted ones.
	WithProposed bool
}

// Direction is which way Walk follows a fact.
type Direction int

const (
	// Out follows facts from subject to object: what the entity depends on.
	Out Direction = iota
	// In follows facts from object to subject: what depends on the entity.
	In
)

// WalkRequest is one Walk.
type WalkRequest struct {
	From       EntityID
	Predicates []string
	Direction  Direction
	// MaxDepth bounds the walk; zero means 10.
	MaxDepth int
	// At is as in FactFilter.
	At time.Time
}

// Step is one fact a Walk crossed, Depth hops from the start (1 for a direct
// fact).
type Step struct {
	Depth int
	Fact  Fact
}

// ChangesRequest is one Changes call.
type ChangesRequest struct {
	Since, Until time.Time
	// Subject limits the changes to facts about one entity, as subject or
	// object.
	Subject EntityID
	// Limit bounds the answer; zero means 1,000.
	Limit int
}

// ChangeKind says whether a fact started or stopped holding.
type ChangeKind string

const (
	// Opened means the fact started holding.
	Opened ChangeKind = "opened"
	// Closed means the fact stopped holding.
	Closed ChangeKind = "closed"
)

// Change is one fact opened or closed at At.
type Change struct {
	Kind ChangeKind
	At   time.Time
	Fact Fact
}
