package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func toRel(relType, to string) *modelv1alpha1.Relation {
	return &modelv1alpha1.Relation{Type: relType, End: &modelv1alpha1.Relation_To{To: to}}
}

// mixedObservation has one bad claim of each kind among good ones: a relation
// with an unregistered type, an attribute claim of the wrong type and an
// entity attribute only the core may claim.
func mixedObservation() *eventv1alpha1.Observation {
	return NewObservation("adapter/test", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{
			Kind: string(KindRepository), Key: "test:repo/a",
			Attributes: map[string]*structpb.Value{
				"name": structpb.NewStringValue("a"), string(PredicateSameAs): structpb.NewStringValue("x"),
			},
		},
		Relations: []*modelv1alpha1.Relation{
			toRel(string(RelOwnedBy), "test:team/a"),
			toRel("likes", "test:team/b"),
			toRel(string(RelOwnedBy), "test:team/c"),
		},
		AttributeClaims: []*modelv1alpha1.AttributeClaim{
			{Predicate: "language", Value: structpb.NewStringValue("Go")},
			{Predicate: "language", Value: structpb.NewNumberValue(7)},
		},
	})
}

func TestClaimProblemsRejectOnlyTheirClaim(t *testing.T) {
	o := mixedObservation()
	err := ValidateObservation(o)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Scope() != ScopeClaim || len(ve.Problems) != 3 {
		t.Fatalf("got %v, want three claim-scoped problems", err)
	}
	for _, p := range ve.Problems {
		if p.Scope != ScopeClaim {
			t.Errorf("problem %s has scope %v, want claim", p, p.Scope)
		}
	}
	if n := DropRejectedClaims(o, err); n != 3 {
		t.Fatalf("dropped %d claims, want 3", n)
	}
	d := o.GetData()
	if len(d.GetRelations()) != 2 || d.GetRelations()[0].GetTo() != "test:team/a" || d.GetRelations()[1].GetTo() != "test:team/c" ||
		len(d.GetAttributeClaims()) != 1 || d.GetAttributeClaims()[0].GetValue().GetStringValue() != "Go" ||
		len(d.GetEntity().GetAttributes()) != 1 || d.GetEntity().GetAttributes()["name"] == nil {
		t.Fatalf("got %v, want only the good claims left", d)
	}
	if err := ValidateObservation(o); err != nil {
		t.Fatalf("got %v after dropping, want a valid observation", err)
	}
}

func TestObservationProblemsRejectTheWholeObservation(t *testing.T) {
	o := mixedObservation()
	o.Data.Entity.Key = "no key" // malformed: not tied to any one claim
	err := ValidateObservation(o)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Scope() != ScopeObservation {
		t.Fatalf("got %v, want observation scope", err)
	}
	if n := DropRejectedClaims(o, err); n != 0 || len(o.GetData().GetRelations()) != 3 {
		t.Fatalf("dropped %d claims, want the observation left alone", n)
	}
	if n := DropRejectedClaims(o, errors.New("other")); n != 0 {
		t.Fatalf("dropped %d claims for another error, want 0", n)
	}
}

func TestRejectedClaimsLeaveTheObservationWideChecks(t *testing.T) {
	// The numeric "language" claim would overlap the good one as a second
	// value of a one predicate, but it is dropped, so it can't cause a
	// cardinality_mismatch that rejects the whole observation.
	o := NewObservation("adapter/test", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(KindRepository), Key: "test:repo/a"},
		AttributeClaims: []*modelv1alpha1.AttributeClaim{
			{Predicate: "language", Value: structpb.NewStringValue("Go")},
			{Predicate: "language", Value: structpb.NewNumberValue(7)},
		},
	})
	var ve *ValidationError
	if err := ValidateObservation(o); !errors.As(err, &ve) || ve.Scope() != ScopeClaim || ve.Has(codeCardinality) {
		t.Fatalf("got %v, want only a claim-scoped type mismatch", err)
	}
}

func TestEveryRejectedClaimIsDroppedBeyondTheProblemCap(t *testing.T) {
	data := &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: string(KindTeam), Key: "x:team/a"}}
	for range 3 * MaxProblems {
		data.Relations = append(data.Relations, toRel("likes", "x:y/z"))
	}
	data.Relations = append(data.Relations, toRel(string(RelMemberOf), "x:team/b"))
	o := NewObservation("adapter/test", time.Unix(0, 0), data)
	err := ValidateObservation(o)
	if n := DropRejectedClaims(o, err); n != 3*MaxProblems || len(o.Data.Relations) != 1 {
		t.Fatalf("dropped %d, %d relations left; want %d dropped and 1 left", n, len(o.Data.Relations), 3*MaxProblems)
	}
}

func TestProblemMessagesCutLongValues(t *testing.T) {
	long := strings.Repeat("é", 1000) // 2000 bytes; the cut must not split a rune
	o := observationWith(nil)
	o.Data.Entity.Kind = long
	o.Data.Entity.Attributes = map[string]*structpb.Value{long: structpb.NewStringValue("x")}
	o.Data.Relations[0].Type = long
	o.Data.Entity.Aliases = []string{"test:repo/" + long, "no-slash" + long}
	err := ValidateObservation(o)
	if err == nil {
		t.Fatal("got no error")
	}
	msg := err.Error()
	if !utf8.ValidString(msg) || !strings.Contains(msg, `…"`) {
		t.Fatalf("got %q, want valid UTF-8 with the long values cut", msg)
	}
	if strings.Contains(msg, strings.Repeat("é", MaxQuoted)) {
		t.Fatalf("got a message with %d bytes, want values cut to about %d", len(msg), MaxQuoted)
	}
	// Each problem holds a few values of at most MaxQuoted bytes, which
	// strconv.Quote may escape at up to four bytes each.
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %T, want a *ValidationError", err)
	}
	for _, p := range ve.Problems {
		if len(p.String()) > 4*MaxQuoted*3 {
			t.Errorf("problem is %d bytes: %.80s…", len(p.String()), p)
		}
	}
	short := fmt.Sprintf("%q %s", clip("short"), clip("short"))
	if short != `"short" short` {
		t.Fatalf("got %s, want short values untouched", short)
	}
}

func TestDecodeObservationReportsUnknownFieldsAsMalformed(t *testing.T) {
	// A CloudEvents extension attribute other than the core's bearingsource,
	// such as traceparent, is an unknown field and so rejected.
	_, err := DecodeObservation([]byte(`{"specversion":"1.0","traceparent":"00-` + strings.Repeat("a", 5000) + `"}`))
	var ve *ValidationError
	if !errors.As(err, &ve) || !ve.Has(codeMalformed) || ve.Scope() != ScopeObservation {
		t.Fatalf("got %v, want a malformed rejection of the whole observation", err)
	}
	if len(err.Error()) > 2*MaxQuoted+100 {
		t.Fatalf("got a %d-byte error, want the adapter's text cut", len(err.Error()))
	}
}
