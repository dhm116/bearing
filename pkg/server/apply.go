package server

import (
	"context"
	"fmt"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

var _ Applier = StoreApplier{}

// StoreApplier is the Applier the server runs: it validates every
// observation and ingests the page into Store in one transaction.
type StoreApplier struct {
	Store graph.Graph
	// Registry is the vocabulary to validate against; nil means model.Core().
	Registry *model.Registry
	// Authority maps a source to its vouching for linked keys
	// (model.LinkAuthority, with the operator's overrides); a source not
	// listed vouches for none.
	Authority map[string]func(model.Key) bool
	// Active is Config.Active (see there: with a non-nil map an unlisted
	// source keeps nothing). Observations of other kinds are dropped and
	// counted, never refused.
	Active map[string][]model.Kind
}

// Apply implements Applier. An invalid observation fails the whole page, as a
// failed write does, so the page is retried and then dead-lettered.
func (a StoreApplier) Apply(ctx context.Context, source string, obs []*eventv1alpha1.Observation, done Completion) error {
	active := a.Active[source]
	if a.Active != nil && len(active) == 0 {
		// Idle, or no longer configured (its pages are still on the log): the
		// read registry still holds retired kinds, so nothing may reach validation.
		return nil
	}
	// The host dropped these already; this covers events already on the log.
	obs = adapter.DropInactive(ctx, a.Registry, source, obs, active)
	for i, o := range obs {
		if err := model.ValidateObservation(a.Registry, o); err != nil {
			return fmt.Errorf("observation %d: %w", i+1, err)
		}
	}
	_, err := a.Store.Ingest(ctx, graph.IngestRequest{
		Source: source, Observations: obs, Authoritative: a.Authority[source],
		Complete: done.Kinds, SyncStarted: done.Started, Active: active,
	})
	return err
}
