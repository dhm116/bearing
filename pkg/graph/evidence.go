package graph

import (
	"time"

	"bearing.example/pkg/model"
)

// EvidenceRequest is one Evidence read (ADR 11).
type EvidenceRequest struct {
	// From are the records the paths start at: record IDs, not clusters.
	From []EntityID
	// Predicates are the facts to follow; empty follows every predicate.
	Predicates []string
	// Reverse follows facts into From instead of out of it. A claim's end is
	// then the fact's subject, and the kindless-record rule reads the
	// predicate's domain instead of its range.
	Reverse bool
	// MaxDepth is how many facts a path may cross; zero means 1, and 10 is
	// the most.
	MaxDepth int
	// At selects the facts that held then; zero means now. Clusters are
	// always today's.
	At time.Time
	// AnswerKind limits claims to those whose end is this kind or a
	// descendant, retired kinds included; empty accepts any. A kindless end
	// counts when every kind its predicate allows there is one of those.
	// Attribute facts have no end and always count.
	AnswerKind model.Kind
}

// Evidence is the claims an Evidence read found and how their ends relate.
type Evidence struct {
	// Claims are ordered by Facts, SameAs, then fact ID.
	Claims []Claim
	// Relations are ordered by A, then B.
	Relations []Relation
	// Records are the records on the claims' paths, plus every record of the
	// end clusters when there are several, so Relations' links resolve; by ID.
	Records []Record
}

// Claim is one asserted fact and the shortest path that reaches it: fewer
// facts, then fewer same_as hops, then smaller record IDs along the path.
type Claim struct {
	// Fact has record-level Subject and Object, unlike Walk's cluster IDs.
	Fact Fact
	// Start is the From record the path starts at.
	Start EntityID
	// End is the record the path ends at; empty for an attribute fact.
	End EntityID
	// Path runs from Start to End: a same_as hop (at most one) before each
	// fact, then the fact.
	Path []PathStep
	// Facts and SameAs count the facts and hops on Path.
	Facts, SameAs int
}

// PathStep is a same_as hop between two records of one cluster, or a fact
// crossed from one record to the next.
type PathStep struct {
	From, To EntityID
	// Fact is set for a fact step; To is empty when it is an attribute.
	Fact *Fact
	// Links are a hop's asserted same_as link IDs, in order From to To.
	Links []string
}

// RelationKind says how two clusters at the ends of claims relate.
type RelationKind string

const (
	// RelationDistinct means an asserted distinct_from separates them.
	RelationDistinct RelationKind = "distinct"
	// RelationAssertedNotJoined means an asserted same_as the store did not apply.
	RelationAssertedNotJoined RelationKind = "asserted_not_joined"
	// RelationProposed means only proposed links, open now, relate them.
	RelationProposed RelationKind = "proposed"
)

// Relation says how two distinct clusters among the claims' ends relate. A
// pair with no open link has none. Scoring proposals is the caller's job.
type Relation struct {
	// A and B are cluster IDs, A < B.
	A, B EntityID
	Kind RelationKind
	// Links are the open links between records of the two clusters: the
	// deciding ones first, then proposals.
	Links []Link
}
