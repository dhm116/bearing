package server

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/pkg/graph"
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

// namesRegistry is Core plus an adapter-declared MANY string attribute, as
// boot builds it.
func namesRegistry(t *testing.T) *model.Registry {
	t.Helper()
	reg, err := model.Core().Extend(nil, []model.Predicate{{
		Name: "zone_names", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING,
		Cardinality: modelv1alpha1.Cardinality_CARDINALITY_MANY, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func teamWith(t *testing.T, names *structpb.Value) *eventv1alpha1.Observation {
	t.Helper()
	o := validObservation(t)
	o.GetData().GetEntity().Kind = "Team"
	o.GetData().GetEntity().Attributes = map[string]*structpb.Value{"zone_names": names}
	o.GetData().Relations = nil
	return o
}

func TestStoreApplierKeepsEveryValueOfADeclaredManyAttribute(t *testing.T) {
	t.Parallel()
	reg := namesRegistry(t)
	d, err := duckstore.Open(t.Context(), duckstore.Config{Now: time.Now, Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	list, _ := structpb.NewList([]any{"a", "b", "c"})
	o := teamWith(t, structpb.NewListValue(list))
	if err := (StoreApplier{Store: d, Registry: reg}).Apply(t.Context(), testSource, []*eventv1alpha1.Observation{o}, Completion{}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Entity(t.Context(), o.GetData().GetEntity().GetKey())
	if err != nil {
		t.Fatal(err)
	}
	facts, err := d.Facts(t.Context(), graph.FactFilter{Subject: e.ID, Predicate: "zone_names"})
	if err != nil || len(facts) != 3 {
		t.Fatalf("got %d facts, %v, want all three tag values kept", len(facts), err)
	}
}

func TestStoreApplierRefusesAnUndeclaredNameSentWithAnotherType(t *testing.T) {
	t.Parallel()
	reg := namesRegistry(t)
	d, err := duckstore.Open(t.Context(), duckstore.Config{Now: time.Now, Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	// This source declares nothing (another adapter declared tags as strings).
	o := teamWith(t, structpb.NewBoolValue(true))
	err = (StoreApplier{Store: d, Registry: reg}).Apply(t.Context(), testSource, []*eventv1alpha1.Observation{o}, Completion{})
	var ve *model.ValidationError
	if !errors.As(err, &ve) || ve.Scope() != model.ScopeClaim {
		t.Fatalf("got %v, want a claim-scoped type mismatch", err)
	}
}
