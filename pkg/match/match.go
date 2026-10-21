// Package match finds records that describe one thing and says how sure it
// is (ADR 7): key rules assert links, matchers propose them, and a
// proposal's score decays with time unless new evidence reinforces it.
// Matchers only propose; nothing here promotes a proposal.
//
// Run is the entry point. A Matcher scores candidate pairs; Transparent is
// the built-in one, and a model-backed matcher (ADR 7's spike) implements
// the same interface.
package match

import (
	"context"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// Matcher scores candidate pairs of records.
type Matcher interface {
	// Rule names the matcher on the links it proposes, such as
	// "matcher/transparent".
	Rule() string
	// Match returns one Judgement per candidate it can score, from what ev
	// says about the records.
	Match(ctx context.Context, cs []Candidate, ev Evidence) ([]Judgement, error)
}

// Judgement is a matcher's verdict on one candidate pair.
type Judgement struct {
	Candidate Candidate
	// Score is between 0 and 1; Run proposes a link at Config.ProposeAt.
	Score float64
	// Features are the inputs and their values, kept as the link's evidence.
	Features map[string]float64
}

// IdentityGraph is the part of graph.Graph that matching reads and writes:
// records and facts in, identity links out.
type IdentityGraph interface {
	Records(ctx context.Context, kind model.Kind) ([]graph.Record, error)
	Facts(ctx context.Context, f graph.FactFilter) ([]graph.Fact, error)
	Links(ctx context.Context, f graph.LinkFilter) ([]graph.Link, error)
	Link(ctx context.Context, ps []graph.Proposal) (graph.LinkResult, error)
	CloseLink(ctx context.Context, id string, at time.Time) error
}

var _ IdentityGraph = graph.Graph(nil)

// pair is an unordered pair of record IDs, smaller first.
type pair struct{ a, b graph.EntityID }

func pairOf(x, y graph.EntityID) pair {
	if y < x {
		x, y = y, x
	}
	return pair{x, y}
}
