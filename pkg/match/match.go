// Package match finds records that describe one thing and says how sure it
// is (ADR 7): key rules assert links, a transparent matcher proposes them,
// and a proposal's score decays with time unless new evidence reinforces it.
// Matchers only propose; nothing here promotes a proposal.
package match

import (
	"context"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// Graph is the part of graph.Graph that matching uses.
type Graph interface {
	Records(ctx context.Context, kind model.Kind) ([]graph.Record, error)
	Facts(ctx context.Context, f graph.FactFilter) ([]graph.Fact, error)
	Links(ctx context.Context, f graph.LinkFilter) ([]graph.Link, error)
	Link(ctx context.Context, ps []graph.Proposal) (graph.LinkResult, error)
	CloseLink(ctx context.Context, id string, at time.Time) error
}

var _ Graph = graph.Graph(nil)

// pair is an unordered pair of record IDs, smaller first.
type pair struct{ a, b graph.EntityID }

func pairOf(x, y graph.EntityID) pair {
	if y < x {
		x, y = y, x
	}
	return pair{x, y}
}
