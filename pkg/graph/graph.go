// Package graph defines the graph Bearing keeps entities and facts in
// (ADR 6): the one interface a store serves it through, and the types its
// reads return. Anything Bearing needs from the graph is a method here, so
// another engine can replace the default (pkg/store opens a store; see
// internal/duckstore).
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

// ErrInvalidDecision means a Decision cannot be carried out as given: an
// unknown kind, a missing field, or a link of the wrong kind or state.
var ErrInvalidDecision = errors.New("invalid decision")

// EntityID identifies a record or a cluster of records (ADR 7). A record's
// ID never changes; a cluster's is its oldest record's, so it changes when
// links join or split the cluster. Store record IDs or keys, not cluster IDs.
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

// Graph is the graph capabilities a store provides: ingest, entity, find,
// facts, walk and changes. Implementations are safe for concurrent use.
type Graph interface {
	// Ingest validates nothing (callers validate with pkg/model first) and
	// writes one source's observations in one transaction: each observation
	// once by ID, the records and keys it names, the identity links its
	// linked keys make (ADR 7), and the facts it opens, refreshes or closes.
	// A failed Ingest writes nothing.
	Ingest(ctx context.Context, r IngestRequest) (IngestResult, error)

	// Entity returns the entity (a cluster of records, ADR 7) that an entity
	// ID, a record ID or a key names. It fails with ErrNotFound when nothing
	// matches.
	Entity(ctx context.Context, ref string) (Entity, error)
	// Find returns entities whose name or keys resemble query, best first.
	// An empty kind matches every kind.
	Find(ctx context.Context, query string, kind model.Kind, limit int) ([]Match, error)
	// Facts returns the facts matching f, ordered by subject, predicate and
	// valid_from.
	Facts(ctx context.Context, f FactFilter) ([]Fact, error)
	// Walk follows facts with the given predicates from an entity, up to
	// MaxDepth hops, and returns each fact it crossed once, at the shallowest
	// depth it was reached. It enumerates paths and never revisits an entity
	// on one path, so cycles end.
	Walk(ctx context.Context, r WalkRequest) ([]Step, error)
	// Changes returns the facts opened or closed in [Since, Until), newest
	// first.
	Changes(ctx context.Context, r ChangesRequest) ([]Change, error)

	// Records returns the records of one kind (every kind when empty), for
	// matchers to find candidate pairs in.
	Records(ctx context.Context, kind model.Kind) ([]Record, error)
	// Links returns the identity links matching f.
	Links(ctx context.Context, f LinkFilter) ([]Link, error)
	// Link records proposals, and assertions from rules the settings name: a
	// new pair opens a link, a known one gains a deposit. Clusters are
	// recomputed for the records an asserted link touches.
	Link(ctx context.Context, ps []Proposal) (LinkResult, error)
	// CloseLink ends a link at a time; a closed asserted same_as splits its
	// cluster. Closing a link Ingest made lasts only until the source says it
	// again; to keep two records apart for good, assert a distinct_from.
	CloseLink(ctx context.Context, id string, at time.Time) error
	// Decide carries out a person's decision on a link in one transaction,
	// so the closing and the asserting land together, and at most once per
	// Decision.ID: a decision applied before changes nothing and returns the
	// zero LinkResult. It fails with ErrNotFound for an unknown link or
	// record and ErrInvalidDecision for one it cannot carry out; a failed
	// Decide writes nothing.
	Decide(ctx context.Context, d Decision) (LinkResult, error)
}

// IngestResult counts what one Ingest wrote.
type IngestResult struct {
	// Observations is how many were new; Duplicates how many were already
	// stored and skipped.
	Observations, Duplicates int
	// Entities is how many entities were created.
	Entities int
	// Opened, Refreshed and Closed count facts.
	Opened, Refreshed, Closed int
}

// Entity is one thing: a service, a repository, a person, a team.
type Entity struct {
	// ID is the cluster's ID.
	ID   EntityID
	Kind model.Kind
	// Name is for people to read; it is not unique.
	Name string
	// Keys are every key of every record in the cluster, sorted.
	Keys []model.Key
	// Records are the cluster's records, one per thing a source described.
	Records []Record
}

// Record is one thing as one source described it: a key and its aliases,
// under one entity ID. Records joined by asserted same_as links form an
// Entity.
type Record struct {
	ID      EntityID
	Cluster EntityID
	Kind    model.Kind
	Name    string
	Keys    []model.Key
	// Sources are the configured sources that observed the record.
	Sources []string
	// LastSeen is the newest observation of the record from any source; zero
	// when none has, or when that observation deleted it (a deletion wins a
	// tie). Matchers date their
	// evidence by it.
	LastSeen time.Time
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
	Value any
	// Qualifiers are a relation's details, such as protocol and port; nil
	// when it has none. They are not part of what the fact says.
	Qualifiers map[string]any
	Status     Status
	// Confidence is between 0 and 1.
	Confidence float64
	// ValidFrom is when a source first observed the fact; ValidTo when it was
	// observed absent, or zero while it holds.
	ValidFrom, ValidTo time.Time
	// Sources are the observations behind the fact, oldest first: per
	// source, the first and the latest, so a fact synced every hour stays
	// small. SourceCount is how many observations there are in all, counted
	// per fact row: an answer that merges rows sums them, and may count one
	// observation twice.
	Sources     []Source
	SourceCount int
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

// IngestRequest is one Ingest call.
type IngestRequest struct {
	// Source is the configured source that delivered the observations.
	Source       string
	Observations []*eventv1alpha1.Observation
	// Authoritative says whether the source vouches for a linked key, from
	// its adapter declaration (model.LinkDeclaration.authority). A linked key
	// it vouches for makes an asserted same_as link; any other, a proposed
	// one. Nil vouches for none.
	Authoritative func(linked model.Key) bool
	// Complete lists the kinds this sync listed in full, set on a sync's last
	// page. Records of those kinds that the source observed before
	// SyncStarted and not since are gone from it: their facts from this
	// source close.
	Complete    []model.Kind
	SyncStarted time.Time
}

// LinkKind is what an identity link says about two records.
type LinkKind string

const (
	// SameAs says two records describe one thing.
	SameAs LinkKind = "same_as"
	// DistinctFrom says they don't. Asserted, it keeps a matcher from
	// proposing the pair and Ingest from linking it: undoing a join for good
	// is closing the same_as and asserting a distinct_from.
	DistinctFrom LinkKind = "distinct_from"
)

// Link is an identity link between two records (ADR 7).
type Link struct {
	ID   string
	A, B EntityID
	Kind LinkKind
	// Status is Asserted (it joins clusters) or Proposed (it waits for
	// review and joins nothing).
	Status Status
	// Rule names what made the link: "linked_ids", "shared_key", a rule from
	// the settings, a matcher, or "person".
	Rule string
	// Deposits are the evidence over time, oldest first; matchers score
	// them (pkg/match).
	Deposits           []Deposit
	ValidFrom, ValidTo time.Time
}

// Deposit is one piece of evidence for (or, when Against, against) a link.
type Deposit struct {
	At      time.Time
	Weight  float64
	Against bool
	// ObservationID is the observation behind it, when there is one.
	ObservationID string
	// Actor is who decided, for a person's decision (an event's
	// Actor.subject); empty for evidence from observations and matchers.
	Actor string
	// Reason is the person's free text, kept as given and never interpreted.
	Reason string
	// Features are a matcher's inputs and their contributions, as evidence.
	Features map[string]float64
}

// LinkFilter selects links. Empty fields match anything.
type LinkFilter struct {
	// ID matches one link; with WithClosed unset it matches only while open.
	ID string
	// Entity matches links touching any record of this entity or record.
	Entity     EntityID
	Kind       LinkKind
	Status     Status
	WithClosed bool
}

// Proposal is one link a matcher or rule writes.
type Proposal struct {
	A, B   EntityID
	Kind   LinkKind
	Status Status
	Rule   string
	// Deposit is the evidence it adds.
	Deposit Deposit
}

// LinkResult counts what one Link call wrote.
type LinkResult struct {
	Opened, Reinforced int
	// Closed counts links Decide closed.
	Closed int
}

// PersonRule is the Rule of the links a person's decision asserts.
const PersonRule = "person"

// DecisionKind is what a person decided about a link.
type DecisionKind string

// The decisions a person can make (ADR 7: a proposed link becomes asserted
// only by a person or a rule the settings name). All are reversible: facts
// stay on their records.
const (
	// Accept asserts a proposed same_as (Decision.LinkID): the records join.
	Accept DecisionKind = "accept"
	// Reject closes a proposed same_as (LinkID) and asserts a distinct_from
	// for its pair, so no matcher or ingest proposes the pair again.
	Reject DecisionKind = "reject"
	// Join asserts a same_as between A and B, proposed or not, and closes a
	// distinct_from between them.
	Join DecisionKind = "join"
	// Split closes the same_as links between A and B and asserts a
	// distinct_from.
	Split DecisionKind = "split"
	// UndoDistinct closes a distinct_from (LinkID): the pair may be
	// proposed again.
	UndoDistinct DecisionKind = "undo_distinct"
)

// Decision is one decision of a person.
type Decision struct {
	// ID makes the decision happen once; the server uses its event's ID.
	ID   string
	Kind DecisionKind
	// LinkID is the link Accept, Reject and UndoDistinct act on.
	LinkID string
	// A and B are the records Join and Split act on, each a record ID, an
	// entity ID or a key.
	A, B string
	// Actor is who decided; required.
	Actor  string
	Reason string
	// At is when; zero means the store's clock.
	At time.Time
}
