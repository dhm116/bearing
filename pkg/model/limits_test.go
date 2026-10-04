package model

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// nested returns a value nested depth objects deep around leaf.
func nested(depth int, leaf *structpb.Value) *structpb.Value {
	v := leaf
	for range depth {
		v = structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{"a": v}})
	}
	return v
}

func observationWith(attrs map[string]*structpb.Value) *eventv1alpha1.Observation {
	return NewObservation("adapter/test", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(KindRepository), Key: "test:repo/a", Attributes: attrs},
		Relations: []*modelv1alpha1.Relation{{
			Type: string(RelOwnedBy), End: &modelv1alpha1.Relation_To{To: "test:team/a"},
			Attributes: map[string]*structpb.Value{"q": attrs["deep"]},
		}},
	})
}

func TestValidateObservationRejectsDeepValues(t *testing.T) {
	ok := observationWith(map[string]*structpb.Value{"deep": nested(MaxValueDepth, structpb.NewNumberValue(1))})
	if err := ValidateObservation(ok); err != nil {
		t.Fatalf("got %v at the depth limit, want nil", err)
	}
	deep := observationWith(map[string]*structpb.Value{"deep": nested(100_000, structpb.NewNumberValue(1))})
	err := ValidateObservation(deep)
	wantCode(t, err, codeMalformed)
	if !strings.Contains(err.Error(), "nested more than 32 deep") {
		t.Fatalf("got %v, want the depth limit named", err)
	}
	if len(err.Error()) > 4096 {
		t.Fatalf("got a %d-byte error, want a short one", len(err.Error()))
	}

	wide := make([]*structpb.Value, MaxValueEntries+1)
	for i := range wide {
		wide[i] = structpb.NewNullValue()
	}
	err = ValidateObservation(observationWith(map[string]*structpb.Value{
		"deep": structpb.NewListValue(&structpb.ListValue{Values: wide}),
	}))
	wantCode(t, err, codeMalformed)
}

// TestValidateObservationIsLinear checks that validating a deep, wide value
// tree costs allocations in proportion to its size, by comparing a tree with
// four times as many nodes.
func TestValidateObservationIsLinear(t *testing.T) {
	tree := func(width int) *eventv1alpha1.Observation {
		fields := map[string]*structpb.Value{}
		for i := range width {
			fields["k"+strconv.Itoa(i)] = nested(MaxValueDepth-1, structpb.NewStringValue("x"))
		}
		return observationWith(map[string]*structpb.Value{"deep": structpb.NewStructValue(&structpb.Struct{Fields: fields})})
	}
	small, large := tree(50), tree(200)
	allocs := func(o *eventv1alpha1.Observation) float64 {
		return testing.AllocsPerRun(3, func() {
			if err := ValidateObservation(o); err != nil {
				t.Fatal(err)
			}
		})
	}
	a, b := allocs(small), allocs(large)
	if ratio := b / a; ratio > 6 {
		t.Fatalf("got %.0f allocations for 4x the nodes vs %.0f (ratio %.1f), want about 4", b, a, ratio)
	}
}

func TestValidationErrorIsCapped(t *testing.T) {
	var rels []*modelv1alpha1.Relation
	for range 50 {
		rels = append(rels, &modelv1alpha1.Relation{Type: "likes", End: &modelv1alpha1.Relation_To{To: "x:y/z"}})
	}
	o := NewObservation("adapter/test", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity:    &modelv1alpha1.Entity{Kind: string(KindTeam), Key: "x:team/a"},
		Relations: rels,
	})
	err := ValidateObservation(o)
	ve, ok := err.(*ValidationError) //nolint:errorlint // returned unwrapped
	if !ok || len(ve.Problems) != MaxProblems || ve.More != 30 || !strings.HasSuffix(err.Error(), "; and 30 more") {
		t.Fatalf("got %v, want %d problems listed and 30 more", err, MaxProblems)
	}
	if !ve.Has(codeNotDeclared) {
		t.Fatal("Has lost the code")
	}
}

func TestValidateAdapterObservationRejectsBearingSource(t *testing.T) {
	o := observationWith(nil)
	if err := ValidateAdapterObservation(o); err != nil {
		t.Fatal(err)
	}
	o.Bearingsource = "github-acme"
	if err := ValidateObservation(o); err != nil {
		t.Fatalf("got %v from the core's own check, want nil", err)
	}
	wantCode(t, ValidateAdapterObservation(o), codeMalformed)
}

func TestValidateRejectsInvalidUTF8(t *testing.T) {
	o := observationWith(map[string]*structpb.Value{"description": structpb.NewStringValue("\xff")})
	o.Data.Entity.Aliases = []string{"test:repo/\xff"}
	err := ValidateObservation(o)
	wantCode(t, err, codeMalformed)
	if !strings.Contains(err.Error(), "aliases[0]: is not valid UTF-8") || !strings.Contains(err.Error(), `attributes["description"]: is not valid UTF-8`) {
		t.Fatalf("got %v, want both the alias and the attribute named", err)
	}
	if _, err := FactID("s", "name", &modelv1alpha1.FactObject{
		Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewStringValue("\xff"),
	}); err == nil {
		t.Fatal("got no error for an invalid UTF-8 value, want one (it would collide with U+FFFD)")
	}
	if _, err := canonicalJSON(map[string]any{"\xfe": 1.0}); err == nil {
		t.Fatal("got no error for an invalid UTF-8 member name, want one")
	}
}

func TestTruncateTimesLeavesInvalidTimestamps(t *testing.T) {
	ts := &timestamppb.Timestamp{Seconds: 1, Nanos: -1500}
	TruncateTimes(ts)
	if ts.GetNanos() != -1500 {
		t.Fatalf("got nanos %d, want -1500 left for validation to reject", ts.GetNanos())
	}
}
