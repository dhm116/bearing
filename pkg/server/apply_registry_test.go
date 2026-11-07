package server

import (
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/pkg/model"
)

func TestStoreApplierValidatesAgainstAConfiguredRegistry(t *testing.T) {
	t.Parallel()
	reg, err := model.Core().Extend([]model.KindDef{{Name: "k8s.Service", Parent: model.KindComponent}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := duckstore.Open(t.Context(), duckstore.Config{Now: time.Now, Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	o := validObservation(t)
	o.GetData().GetEntity().Kind = "k8s.Service"
	o.GetData().GetEntity().Attributes = nil
	o.GetData().Relations = nil
	page := []*eventv1alpha1.Observation{o}
	if err := (StoreApplier{Store: d, Registry: model.Core()}).Apply(t.Context(), testSource, page, Completion{}); err == nil {
		t.Fatal("applied a configured kind against the core registry")
	}
	if err := (StoreApplier{Store: d, Registry: reg}).Apply(t.Context(), testSource, page, Completion{}); err != nil {
		t.Fatalf("got %v, want the configured kind accepted", err)
	}
	if e, err := d.Entity(t.Context(), o.GetData().GetEntity().GetKey()); err != nil || e.Kind != "k8s.Service" {
		t.Errorf("got %+v, %v, want a k8s.Service record", e, err)
	}
}
