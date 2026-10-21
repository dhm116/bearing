package server

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
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

func TestStoreApplierChecksAttributesAgainstTheSourcesDeclaration(t *testing.T) {
	o := validObservation(t)
	o.GetData().GetEntity().Attributes = map[string]*structpb.Value{"cluster_zone": structpb.NewStringValue("eu-1")}
	page := []*eventv1alpha1.Observation{o}
	declared := map[string]map[string]model.Predicate{testSource: {"cluster_zone": {
		Name: "cluster_zone", Type: modelv1alpha1.ValueType_VALUE_TYPE_BOOL, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE,
	}}}
	if err := (StoreApplier{Store: newDuck(t, time.Now), Declared: declared}).Apply(t.Context(), testSource, page, Completion{}); err == nil {
		t.Fatal("applied a string where the source declares a bool")
	}
	declared[testSource]["cluster_zone"] = model.Predicate{
		Name: "cluster_zone", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE,
	}
	if err := (StoreApplier{Store: newDuck(t, time.Now), Declared: declared}).Apply(t.Context(), testSource, page, Completion{}); err != nil {
		t.Fatalf("got %v, want the declared string accepted", err)
	}
}
