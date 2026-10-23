package server

import (
	"context"
	"fmt"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

var _ Applier = StoreApplier{}

// StoreApplier is the Applier the server runs: it validates every
// observation and ingests the page into Store in one transaction.
type StoreApplier struct {
	Store graph.Graph
	// Declared maps a source to the attributes its adapter declares beyond
	// the registry (model.DeclaredAttributes); a source not listed has none.
	Declared map[string]map[string]model.Predicate
	// Authority maps a source to its vouching for linked keys
	// (model.LinkAuthority, with the operator's overrides); a source not
	// listed vouches for none.
	Authority map[string]func(model.Key) bool
}

// Apply implements Applier. An invalid observation fails the whole page, as a
// failed write does, so the page is retried and then dead-lettered.
func (a StoreApplier) Apply(ctx context.Context, source string, obs []*eventv1alpha1.Observation, done Completion) error {
	for i, o := range obs {
		if err := model.ValidateObservationWith(o, a.Declared[source]); err != nil {
			return fmt.Errorf("observation %d: %w", i+1, err)
		}
	}
	_, err := a.Store.Ingest(ctx, graph.IngestRequest{
		Source: source, Observations: obs, Authoritative: a.Authority[source],
		Complete: done.Kinds, SyncStarted: done.Started,
	})
	return err
}
