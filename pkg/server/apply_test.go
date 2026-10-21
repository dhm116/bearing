package server

import (
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
)

func TestStoreApplierIngestsAValidPageAndRefusesAnInvalidOne(t *testing.T) {
	d := newDuck(t, time.Now)
	a := StoreApplier{Store: d}
	good := validObservation(t)
	bad := validObservation(t)
	bad.Id = "bad"
	bad.GetData().GetEntity().Key = ""
	if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{good, bad}); err == nil {
		t.Fatal("applied a page with an invalid observation")
	}
	key := good.GetData().GetEntity().GetKey()
	if _, err := d.Entity(t.Context(), key); err == nil {
		t.Fatal("a refused page wrote its valid observation")
	}
	if err := a.Apply(t.Context(), testSource, []*eventv1alpha1.Observation{good}); err != nil {
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
