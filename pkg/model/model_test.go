package model

import (
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestKeyParse(t *testing.T) {
	tests := []struct {
		key                          Key
		namespace, keyType, external string
		wantErr                      bool
	}{
		{key: "github:user/jdoe", namespace: "github", keyType: "user", external: "jdoe"},
		{key: "github:repo_node/R_kgDOH1a2bw", namespace: "github", keyType: "repo_node", external: "R_kgDOH1a2bw"},
		{key: "github:repo/acme/payments-api", namespace: "github", keyType: "repo", external: "acme/payments-api"},
		{key: "ghes-acme:team/acme/x", namespace: "ghes-acme", keyType: "team", external: "acme/x"},
		{key: "aws:resource/arn:aws:s3:::bucket", namespace: "aws", keyType: "resource", external: "arn:aws:s3:::bucket"},
		{key: "github:user", wantErr: true},
		{key: "GitHub:user/jdoe", wantErr: true},
		{key: "github:user/", wantErr: true},
		{key: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.key), func(t *testing.T) {
			ns, kt, ext, err := tt.key.Parse()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got no error for %q, want one", tt.key)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ns != tt.namespace || kt != tt.keyType || ext != tt.external {
				t.Fatalf("got %q %q %q, want %q %q %q", ns, kt, ext, tt.namespace, tt.keyType, tt.external)
			}
			if got := NewKey(ns, kt, ext); got != tt.key {
				t.Fatalf("NewKey got %q, want %q", got, tt.key)
			}
		})
	}
}

func TestNewObservationValidates(t *testing.T) {
	at := time.Date(2026, 9, 28, 1, 30, 0, 123456789, time.FixedZone("x", 3600))
	o := NewObservation("adapter/test", at, &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: string(KindTeam), Key: "github:team/acme/payments"},
		Relations: []*modelv1alpha1.Relation{{
			Type: string(RelMemberOf), End: &modelv1alpha1.Relation_To{To: "github:team/acme/engineering"},
		}},
	})
	if err := ValidateObservation(o); err != nil {
		t.Fatal(err)
	}
	if want := "github:team/acme/payments@2026-09-28T00:30:00.123456Z"; o.GetId() != want {
		t.Fatalf("got id %q, want %q", o.GetId(), want)
	}
	if got := o.GetTime().GetNanos(); got != 123456000 {
		t.Fatalf("got nanos %d, want 123456000 (truncated to microseconds)", got)
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test fixtures under testdata
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFixtureNodeIDsAreCanonical keeps made-up GitHub node IDs in fixtures
// decodable: next-format IDs are URL-safe base64 of a MessagePack array
// whose trailing bits are zero, which a typed-in suffix like "b3" breaks.
func TestFixtureNodeIDsAreCanonical(t *testing.T) {
	idPattern := regexp.MustCompile(`github:(?:repo|user|team)_node/[A-Z]_([A-Za-z0-9_-]+)`)
	checked := 0
	for _, dir := range []string{"../../testdata/observations", "../../testdata/acme"} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			for _, m := range idPattern.FindAllStringSubmatch(string(readFixture(t, path)), -1) {
				checked++
				b, err := base64.RawURLEncoding.Strict().DecodeString(m[1])
				if err != nil || len(b) < 2 || b[0]&0xf0 != 0x90 || b[1] != 0 {
					t.Errorf("%s: node ID %s is not a canonical next-format ID (%v)", path, m[0], err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked == 0 {
		t.Fatal("found no GitHub node IDs in the fixtures; has the key pattern changed?")
	}
}

func TestValidObservationsDecodeAndRoundTrip(t *testing.T) {
	files, err := filepath.Glob("../../testdata/observations/valid/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("got %d fixtures (%v), want some", len(files), err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			o, err := DecodeObservation(readFixture(t, file))
			if err != nil {
				t.Fatal(err)
			}
			b, err := EncodeJSON(o)
			if err != nil {
				t.Fatal(err)
			}
			again, err := DecodeObservation(b)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(o, again) {
				t.Fatalf("round trip changed the observation:\n got %s\nwant %v", b, o)
			}
		})
	}
}

// An empty linked_ids with linked_ids_complete is "read, and there are none":
// the flag survives a round trip through ProtoJSON, which drops the empty list.
func TestLinkedIDsCompleteSurvivesWithAnEmptyList(t *testing.T) {
	o, err := DecodeObservation(readFixture(t, "../../testdata/observations/valid/8-person-links-cleared.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeJSON(o)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeObservation(b)
	if err != nil {
		t.Fatal(err)
	}
	if e := again.GetData().GetEntity(); len(e.GetLinkedIds()) != 0 || !e.GetLinkedIdsComplete() {
		t.Fatalf("got linked_ids %v, complete %v, want none and complete", e.GetLinkedIds(), e.GetLinkedIdsComplete())
	}
}

// Unflagged, a list only adds evidence.
func TestLinkedIDsAreAddOnlyByDefault(t *testing.T) {
	o, err := DecodeObservation(readFixture(t, "../../testdata/observations/valid/5-person-linked-ids.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e := o.GetData().GetEntity(); len(e.GetLinkedIds()) == 0 || e.GetLinkedIdsComplete() {
		t.Fatalf("got linked_ids %v, complete %v, want some and not complete", e.GetLinkedIds(), e.GetLinkedIdsComplete())
	}
}

// invalidFixtures names the one rejection code each invalid fixture must
// produce. REJECTION_CODE_UNSPECIFIED means it must fail to decode.
var invalidFixtures = map[string]modelv1alpha1.RejectionCode{
	"attribute-in-both-places.json":             codeDuplicateClaim,
	"attribute-claim-absent-without-value.json": codeMalformed,
	"exists-attribute.json":                     codeMalformed,
	"one-attribute-overlap.json":                codeCardinality,
	"one-relation-overlap.json":                 codeCardinality,
	"undefined-enum.json":                       codeMalformed,
	"bad-key.json":                              codeMalformed,
	"bad-relation-key.json":                     codeMalformed,
	"cardinality-mismatch.json":                 codeCardinality,
	"confidence-out-of-range.json":              codeInvalidValue,
	"confidence-zero.json":                      codeInvalidValue,
	"core-predicate.json":                       codeCorePredicate,
	"domain-mismatch.json":                      codeDomainMismatch,
	"duplicate-claim.json":                      codeDuplicateClaim,
	"missing-entity.json":                       codeMalformed,
	"missing-relation-end.json":                 codeMalformed,
	"missing-snapshot-direction.json":           codeMalformed,
	"missing-time.json":                         codeMalformed,
	"non-canonical-time.json":                   codeInvalidValue,
	"type-mismatch.json":                        codeTypeMismatch,
	"unknown-field.json":                        codeMalformed,
	"unknown-kind.json":                         codeNotDeclared,
	"unknown-relation.json":                     codeNotDeclared,
	"valid-to-before-valid-from.json":           codeInvalidInterval,
	"valid-to-equals-valid-from.json":           codeInvalidInterval,
	"wrong-type.json":                           codeMalformed,
}

// claimFixtures are the invalid fixtures whose problems reject only a claim;
// the others reject the whole observation.
var claimFixtures = map[string]bool{
	"confidence-out-of-range.json":    true,
	"confidence-zero.json":            true,
	"core-predicate.json":             true,
	"domain-mismatch.json":            true,
	"non-canonical-time.json":         true,
	"type-mismatch.json":              true,
	"unknown-relation.json":           true,
	"valid-to-before-valid-from.json": true,
	"valid-to-equals-valid-from.json": true,
}

func TestInvalidObservationsAreRejected(t *testing.T) {
	files, err := filepath.Glob("../../testdata/observations/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(invalidFixtures) {
		t.Fatalf("got %d invalid fixtures, want %d: every fixture needs an entry in invalidFixtures", len(files), len(invalidFixtures))
	}
	for _, file := range files {
		name := filepath.Base(file)
		t.Run(name, func(t *testing.T) {
			want, ok := invalidFixtures[name]
			if !ok {
				t.Fatalf("%s has no entry in invalidFixtures", name)
			}
			// unknown-field.json fails in decoding, which DecodeObservation
			// reports as malformed; every other fixture decodes.
			b := readFixture(t, file)
			if err := DecodeJSON(b, &eventv1alpha1.Observation{}); (err != nil) != (name == "unknown-field.json") {
				t.Fatalf("decoding: got %v", err)
			}
			_, err := DecodeObservation(b)
			ve, ok := err.(*ValidationError) //nolint:errorlint // ValidateObservation returns it unwrapped
			if !ok {
				t.Fatalf("got %v, want a *ValidationError", err)
			}
			if got := ve.Codes(); !slices.Equal(got, []modelv1alpha1.RejectionCode{want}) {
				t.Fatalf("got codes %v (%v), want only %v", got, err, want)
			}
			wantScope := ScopeObservation
			if claimFixtures[name] {
				wantScope = ScopeClaim
			}
			if got := ve.Scope(); got != wantScope {
				t.Fatalf("got scope %v, want %v", got, wantScope)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	o := &eventv1alpha1.Observation{Data: &modelv1alpha1.ObservationData{
		Entity:    &modelv1alpha1.Entity{Kind: "Widget", Key: "nope"},
		Relations: []*modelv1alpha1.Relation{{Type: "likes", End: &modelv1alpha1.Relation_To{To: "also-nope"}}},
	}}
	err := ValidateObservation(o)
	if err == nil {
		t.Fatal("got no error, want one")
	}
	for _, want := range []string{"specversion", "id: is required", "data.entity.kind", "data.entity.key", "data.relations[0].type", "data.relations[0].to"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateChecksAttributeValues(t *testing.T) {
	nan := structpb.NewNumberValue(0)
	nan.Kind = &structpb.Value_NumberValue{NumberValue: math.NaN()}
	tests := []struct {
		name string
		kind Kind
		attr string
		v    *structpb.Value
		want modelv1alpha1.RejectionCode
	}{
		{"nan in unregistered", KindRepository, "score", nan, codeInvalidValue},
		{"relation as attribute", KindRepository, "owned_by", structpb.NewStringValue("x"), codeTypeMismatch},
		{"core predicate", KindPerson, "same_as", structpb.NewStringValue("x"), codeCorePredicate},
		{"bad name", KindPerson, "Login", structpb.NewStringValue("x"), codeMalformed},
		{"list element type", KindRepository, "topics", mustValue(t, []any{"a", 1.0}), codeTypeMismatch},
		{"json list ok", KindCloudResource, "tags", mustValue(t, []any{"a", 1.0}), 0},
		{"null ok", KindRepository, "language", structpb.NewNullValue(), 0},
		{"canonical time ok", KindIncident, "started_at", TimeValue(time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)), 0},
		{"time not a string", KindIncident, "started_at", structpb.NewNumberValue(1), codeTypeMismatch},
		{"bad time", KindIncident, "started_at", structpb.NewStringValue("yesterday"), codeInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NewObservation("adapter/test", time.Unix(0, 0), &modelv1alpha1.ObservationData{
				Entity: &modelv1alpha1.Entity{
					Kind: string(tt.kind), Key: "test:x/1",
					Attributes: map[string]*structpb.Value{tt.attr: tt.v},
				},
			})
			err := ValidateObservation(o)
			if tt.want == 0 {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			ve, ok := err.(*ValidationError) //nolint:errorlint // returned unwrapped
			if !ok || !ve.Has(tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCheckObservedAt(t *testing.T) {
	ingest := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := NewObservation("adapter/test", ingest.Add(5*time.Minute), &modelv1alpha1.ObservationData{})
	if err := CheckObservedAt(o, ingest); err != nil {
		t.Fatalf("got %v at exactly the limit, want nil", err)
	}
	o.Time = timestamppb.New(ingest.Add(5*time.Minute + time.Microsecond))
	err := CheckObservedAt(o, ingest)
	ve, ok := err.(*ValidationError) //nolint:errorlint // returned unwrapped
	if !ok || !ve.Has(codeObservedAtInFuture) {
		t.Fatalf("got %v, want observed_at_in_future", err)
	}
}

func TestTruncateTimesReachesNestedTimestamps(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 999_999_999, time.UTC)
	o := &eventv1alpha1.ObservationsEmitted{Observations: []*eventv1alpha1.Observation{{
		Time: timestamppb.New(at),
		Data: &modelv1alpha1.ObservationData{
			Relations:       []*modelv1alpha1.Relation{{ValidFrom: timestamppb.New(at)}},
			AttributeClaims: []*modelv1alpha1.AttributeClaim{{ValidTo: timestamppb.New(at)}},
		},
	}}}
	TruncateTimes(o)
	obs := o.GetObservations()[0]
	for _, ts := range []*timestamppb.Timestamp{obs.GetTime(), obs.GetData().GetRelations()[0].GetValidFrom(), obs.GetData().GetAttributeClaims()[0].GetValidTo()} {
		if ts.GetNanos() != 999_999_000 {
			t.Fatalf("got nanos %d, want 999999000", ts.GetNanos())
		}
	}
}

func TestShortName(t *testing.T) {
	tests := []struct {
		e    protoreflect.Enum
		want string
	}{
		{modelv1alpha1.FactStatus_FACT_STATUS_ASSERTED, "asserted"},
		{modelv1alpha1.ValueType_VALUE_TYPE_STRING, "string"},
		{modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED, "not_declared"},
		{modelv1alpha1.MergeRule_MERGE_RULE_CO_REPORTED_IDS, "co_reported_ids"},
		{modelv1alpha1.KeyCase_KEY_CASE_INSENSITIVE, "insensitive"},
	}
	for _, tt := range tests {
		if got := ShortName(tt.e); got != tt.want {
			t.Errorf("ShortName(%v) got %q, want %q", tt.e, got, tt.want)
		}
	}
	if got := ShortName(modelv1alpha1.FactStatus(99)); got != "" {
		t.Errorf("got %q for an unknown value, want empty", got)
	}
}

func TestRegistry(t *testing.T) {
	name, ok := LookupPredicate("name")
	if !ok || name.Relation || name.Type != tString || !name.InDomain(KindTeam) || name.InDomain(KindComponent) {
		t.Fatalf("got %+v, want a string attribute of Team and not Component", name)
	}
	owned, ok := LookupPredicate(string(RelOwnedBy))
	if !ok || !owned.Relation || !owned.InDomain(KindDocument) || owned.InRange(KindRepository) || owned.Conflict != setC {
		t.Fatalf("got %+v, want owned_by: any to Team or Person, conflict set", owned)
	}
	if p, _ := LookupPredicate("default_branch"); p.Conflict != oneC {
		t.Fatalf("default_branch conflict got %v, want one", p.Conflict)
	}
	if _, ok := LookupPredicate(PredicateSameAs); ok || !IsCorePredicate(PredicateSameAs) {
		t.Fatal("same_as must be a core predicate outside the registry")
	}
	for _, k := range Kinds {
		if !k.Valid() {
			t.Errorf("%s is not valid", k)
		}
	}
	if Kind("Widget").Valid() {
		t.Error("Widget is valid, want invalid")
	}
}

func mustValue(t *testing.T, v any) *structpb.Value {
	t.Helper()
	pv, err := structpb.NewValue(v)
	if err != nil {
		t.Fatal(err)
	}
	return pv
}

// A source that declares an attribute the registry doesn't have gets the
// same checks for it as for a registered one.
func TestValidateObservationWithAppliesDeclaredAttributes(t *testing.T) {
	declared := map[string]Predicate{
		"score": {Name: "test.score", Type: modelv1alpha1.ValueType_VALUE_TYPE_FLOAT, Cardinality: one},
		"tags2": {Name: "test.tags2", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Cardinality: many},
	}
	build := func(claims []*modelv1alpha1.AttributeClaim, snapshots ...*modelv1alpha1.SnapshotScope) *eventv1alpha1.Observation {
		return NewObservation("adapter/test", time.Unix(0, 0), &modelv1alpha1.ObservationData{
			Entity:          &modelv1alpha1.Entity{Kind: string(KindRepository), Key: "test:x/1"},
			AttributeClaims: claims,
			Snapshots:       snapshots,
		})
	}
	claim := func(pred string, v *structpb.Value) *modelv1alpha1.AttributeClaim {
		return &modelv1alpha1.AttributeClaim{Predicate: pred, Value: v}
	}
	tests := []struct {
		name string
		obs  *eventv1alpha1.Observation
		want modelv1alpha1.RejectionCode // 0: valid
	}{
		{"a value of the declared type", build([]*modelv1alpha1.AttributeClaim{claim("score", structpb.NewNumberValue(1))}), 0},
		{"a value of the wrong type", build([]*modelv1alpha1.AttributeClaim{claim("score", structpb.NewStringValue("high"))}), codeTypeMismatch},
		{"two values of a single-valued attribute", build([]*modelv1alpha1.AttributeClaim{
			claim("score", structpb.NewNumberValue(1)), claim("score", structpb.NewNumberValue(2)),
		}), codeCardinality},
		{"many values of a many attribute", build([]*modelv1alpha1.AttributeClaim{
			claim("tags2", structpb.NewStringValue("a")), claim("tags2", structpb.NewStringValue("b")),
		}), 0},
		{"a snapshot of an attribute it lists", build(nil, &modelv1alpha1.SnapshotScope{
			Direction: modelv1alpha1.Direction_DIRECTION_OUT, Predicates: []string{"tags2"},
		}), 0},
		{"an attribute nobody declared", build([]*modelv1alpha1.AttributeClaim{claim("other", structpb.NewNumberValue(1))}), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateObservationWith(tt.obs, declared)
			if tt.want == 0 {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			ve, ok := err.(*ValidationError) //nolint:errorlint // returned unwrapped
			if !ok || !ve.Has(tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	// Without the declaration the same observation is checked as before.
	o := build([]*modelv1alpha1.AttributeClaim{claim("score", structpb.NewStringValue("high"))})
	if err := ValidateObservation(o); err != nil {
		t.Fatalf("got %v, want an unregistered attribute left unchecked", err)
	}
	if err := ValidateObservationWith(o, nil); err != nil {
		t.Fatalf("got %v, want nil declarations to behave as ValidateObservation", err)
	}
}

func TestJCSOrdersKeysAndRefusesWhatIsNotJSON(t *testing.T) {
	a := mustValue(t, map[string]any{"b": 1.0, "a": []any{"x", true, nil}})
	b := mustValue(t, map[string]any{"a": []any{"x", true, nil}, "b": 1.0})
	got, err := JCS(a)
	if err != nil || string(got) != `{"a":["x",true,null],"b":1}` {
		t.Fatalf("got %s, %v, want keys sorted and 1.0 written as 1", got, err)
	}
	if again, _ := JCS(b); string(again) != string(got) {
		t.Fatalf("got %s and %s, want one form for equal values", got, again)
	}
	nan := &structpb.Value{Kind: &structpb.Value_NumberValue{NumberValue: math.NaN()}}
	nested := structpb.NewListValue(&structpb.ListValue{Values: []*structpb.Value{nan}})
	inStruct := structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{"x": nan}})
	for name, v := range map[string]*structpb.Value{
		"NaN": nan, "NaN in a list": nested, "NaN in an object": inStruct, "invalid UTF-8": structpb.NewStringValue("\xff"),
	} {
		if _, err := JCS(v); err == nil {
			t.Errorf("%s: got no error, want one", name)
		}
	}
}
