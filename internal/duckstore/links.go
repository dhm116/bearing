package duckstore

import (
	"context"
	"errors"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// errNotYet marks the identity-link methods that step 4's identity stream
// implements (ADR 7); they exist so the package builds against graph.Graph.
var errNotYet = errors.New("identity links: not implemented yet")

// Records implements graph.Graph.
func (s *Store) Records(context.Context, model.Kind) ([]graph.Record, error) { return nil, errNotYet }

// Links implements graph.Graph.
func (s *Store) Links(context.Context, graph.LinkFilter) ([]graph.Link, error) { return nil, errNotYet }

// Link implements graph.Graph.
func (s *Store) Link(context.Context, []graph.Proposal) (graph.LinkResult, error) {
	return graph.LinkResult{}, errNotYet
}

// CloseLink implements graph.Graph.
func (s *Store) CloseLink(context.Context, string, time.Time) error { return errNotYet }
