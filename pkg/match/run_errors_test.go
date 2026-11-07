package match

import (
	"context"
	"errors"
	"testing"

	"bearing.example/pkg/graph"
)

var errGraphDown = errors.New("graph down")

// downGraph is the fake with one read that fails.
type downGraph struct {
	*fake
	facts, links, openProposals, link bool
}

func (d downGraph) Link(ctx context.Context, ps []graph.Proposal) (graph.LinkResult, error) {
	if d.link {
		return graph.LinkResult{}, errGraphDown
	}
	return d.fake.Link(ctx, ps)
}

func (d downGraph) Facts(ctx context.Context, f graph.FactFilter) ([]graph.Fact, error) {
	if d.facts {
		return nil, errGraphDown
	}
	return d.fake.Facts(ctx, f)
}

// Links fails for the read of every link, or only for the last read Run
// makes, of open proposals.
func (d downGraph) Links(ctx context.Context, f graph.LinkFilter) ([]graph.Link, error) {
	if d.links && f.WithClosed || d.openProposals && !f.WithClosed {
		return nil, errGraphDown
	}
	return d.fake.Links(ctx, f)
}

func TestRunReturnsTheGraphsReadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		g    downGraph
	}{
		{"facts", downGraph{fake: newFake(), facts: true}},
		{"link", downGraph{fake: newFake(), link: true}},
		{"links", downGraph{fake: newFake(), links: true}},
		{"open proposals", downGraph{fake: newFake(), openProposals: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := Run(t.Context(), tt.g, Config{})
			if !errors.Is(err, errGraphDown) {
				t.Fatalf("got %v, want an error wrapping the graph's", err)
			}
			if res.Proposed != 0 || res.Closed != 0 {
				t.Errorf("got %+v, want an empty result with the error", res)
			}
		})
	}
}
