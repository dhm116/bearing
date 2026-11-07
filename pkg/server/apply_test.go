package server

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/model"
)

func TestStoreApplierIngestsAValidPageAndRefusesAnInvalidOne(t *testing.T) {
	d := newDuck(t, time.Now)
	a := StoreApplier{Store: d}
	good := validObservation(t)
	bad := validObservation(t)
	bad.Id = "bad"
	bad.GetData().GetEntity().Key = ""
	if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{good, bad}, Completion{}); err == nil {
		t.Fatal("applied a page with an invalid observation")
	}
	key := good.GetData().GetEntity().GetKey()
	if _, err := d.Entity(t.Context(), key); err == nil {
		t.Fatal("a refused page wrote its valid observation")
	}
	if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{good}, Completion{}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Entity(t.Context(), key)
	if err != nil {
		t.Fatalf("got %v, want the entity %s", err, key)
	}
	if e.ID == "" || e.Kind == "" {
		t.Errorf("got entity %+v", e)
	}
}

func droppedCounts(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "bearing.ingest.observations.dropped" {
				continue
			}
			for _, dp := range sum.DataPoints {
				src, _ := dp.Attributes.Value("bearing.source")
				kind, _ := dp.Attributes.Value("bearing.kind")
				reason, _ := dp.Attributes.Value("bearing.reason")
				out[src.AsString()+"/"+kind.AsString()+"/"+reason.AsString()] += dp.Value
			}
		}
	}
	return out
}

func TestStoreApplierDropsAndCountsObservationsOfInactiveKinds(t *testing.T) {
	// Not parallel: the counter is read from the shared test meter, keyed by a source of its own.
	const src = "drop-source"
	kept := validObservation(t)
	kind := model.Kind(kept.GetData().GetEntity().GetKind())
	other := validObservation(t)
	other.Id = "other"
	other.GetData().GetEntity().Key = "github:repo/acme/other"
	other.GetData().GetEntity().Kind = string(model.KindTeam)
	// Invalid, but of an inactive kind: dropped, so it does not fail the page.
	bad := validObservation(t)
	bad.Id = "bad"
	bad.GetData().GetEntity().Kind = string(model.KindPerson)
	bad.GetData().GetEntity().Key = ""
	d := newDuck(t, time.Now)
	a := StoreApplier{Store: d, Active: map[string][]model.Kind{src: {kind}}}
	before := droppedCounts(t)
	if err := a.Apply(t.Context(), src, []*eventv1alpha1.Observation{kept, other, bad}, Completion{}); err != nil {
		t.Fatalf("got %v, want the page ingested without the inactive kinds", err)
	}
	if _, err := d.Entity(t.Context(), kept.GetData().GetEntity().GetKey()); err != nil {
		t.Errorf("the active observation is not in the graph: %v", err)
	}
	if _, err := d.Entity(t.Context(), other.GetData().GetEntity().GetKey()); err == nil {
		t.Error("an inactive kind's observation was ingested")
	}
	after := droppedCounts(t)
	for _, k := range []string{src + "/Team/inactive", src + "/Person/inactive"} {
		if got := after[k] - before[k]; got != 1 {
			t.Errorf("dropped[%s] rose by %d, want 1", k, got)
		}
	}
	if len(after) != len(before)+2 {
		t.Errorf("got counters %v, want only the two dropped kinds added to %v", after, before)
	}
}

func TestStoreApplierKeepsEverythingWhenTheSourceIsNotListed(t *testing.T) {
	t.Parallel()
	for name, active := range map[string]map[string][]model.Kind{
		"nil map":      nil,
		"other source": {"elsewhere": {model.KindTeam}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDuck(t, time.Now)
			o := validObservation(t)
			if err := (StoreApplier{Store: d, Active: active}).Apply(t.Context(), testSource, []*eventv1alpha1.Observation{o}, Completion{}); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Entity(t.Context(), o.GetData().GetEntity().GetKey()); err != nil {
				t.Fatalf("got %v, want the observation ingested", err)
			}
		})
	}
}
