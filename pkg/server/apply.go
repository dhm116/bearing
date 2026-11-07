package server

import (
	"context"
	"fmt"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
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
	// Declared maps a source to the attributes its adapter declares beyond
	// the registry (model.DeclaredAttributes); a source not listed has none.
	Declared map[string]map[string]model.Predicate
	// Authority maps a source to its vouching for linked keys
	// (model.LinkAuthority, with the operator's overrides); a source not
	// listed vouches for none.
	Authority map[string]func(model.Key) bool
	// Active maps a source to the kinds it keeps. A source not listed (or a
	// nil map) keeps everything; one listed with no kinds keeps nothing.
	// Observations of other kinds are dropped and counted, never refused.
	Active map[string][]model.Kind
}

// Apply implements Applier. An invalid observation fails the whole page, as a
// failed write does, so the page is retried and then dead-lettered.
func (a StoreApplier) Apply(ctx context.Context, source string, obs []*eventv1alpha1.Observation, done Completion) error {
	active, filtered := a.Active[source]
	if filtered {
		obs = dropInactive(ctx, source, obs, active)
	}
	for i, o := range obs {
		if err := model.ValidateObservationWith(a.Registry, o, a.Declared[source]); err != nil {
			return fmt.Errorf("observation %d: %w", i+1, err)
		}
	}
	if filtered && len(active) == 0 {
		return nil // nothing is kept, and an empty Active would mean everything
	}
	_, err := a.Store.Ingest(ctx, graph.IngestRequest{
		Source: source, Observations: obs, Authoritative: a.Authority[source],
		Complete: done.Kinds, SyncStarted: done.Started, Active: active,
	})
	return err
}

// dropInactive returns the observations whose kind the source keeps,
// counting the rest. An invalid page must not fail for a kind nobody asked
// for, so this runs before validation.
func dropInactive(ctx context.Context, source string, obs []*eventv1alpha1.Observation, active []model.Kind) []*eventv1alpha1.Observation {
	kept := make([]*eventv1alpha1.Observation, 0, len(obs))
	for _, o := range obs {
		k := model.Kind(o.GetData().GetEntity().GetKind())
		if k == "" || slices.Contains(active, k) { // no kind: validation decides
			kept = append(kept, o)
			continue
		}
		observationsDropped.Add(ctx, 1, metric.WithAttributes(attrSourceName.String(source),
			attribute.String("bearing.kind", model.Clip(string(k))), attribute.String(string(attrReason), "inactive")))
	}
	return kept
}
