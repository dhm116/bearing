package adapter

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/model"
)

var observationsDropped = mustCounter("bearing.ingest.observations.dropped",
	"Observations dropped before validation because their kind is not active for the source, by source, kind (other when the registry does not know it) and reason (inactive).", "{observation}")

// otherKind labels a dropped kind the registry does not know: the adapter
// chose the text, so it must not become a metric label.
const otherKind = "other"

// normalizeKinds is the enabled kinds as the host sends them: sorted, each
// once. It is the one place that decides that.
func normalizeKinds(kinds []model.Kind) []model.Kind {
	out := slices.Clone(kinds)
	slices.Sort(out)
	return slices.Compact(out)
}

// DropInactive returns obs without the observations whose kind is not in
// enabled, counting each as dropped for source. An observation with no kind
// stays for validation to judge, and an empty enabled means no filter. It
// runs before validation so a kind the vocabulary lacks (optional and not
// enabled, or retired) cannot fail its page; the dropped record is never
// stored.
func DropInactive(ctx context.Context, reg *model.Registry, source string, obs []*eventv1alpha1.Observation, enabled []model.Kind) []*eventv1alpha1.Observation {
	if len(enabled) == 0 {
		return obs
	}
	if reg == nil {
		reg = model.Core()
	}
	kept := make([]*eventv1alpha1.Observation, 0, len(obs))
	for _, o := range obs {
		k := model.Kind(o.GetData().GetEntity().GetKind())
		if k == "" || slices.Contains(enabled, k) {
			kept = append(kept, o)
			continue
		}
		label := otherKind
		if reg.Valid(k) {
			label = string(k)
		}
		observationsDropped.Add(ctx, 1, metric.WithAttributes(attribute.String("bearing.source", source),
			attribute.String("bearing.kind", label), attribute.String("bearing.reason", "inactive")))
	}
	return kept
}
