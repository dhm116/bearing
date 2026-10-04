package model

import (
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestReferenceDeclarationsValidate(t *testing.T) {
	files, err := filepath.Glob("../../testdata/declarations/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("got %d declaration fixtures (%v), want some", len(files), err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			d := &modelv1alpha1.AdapterDeclaration{}
			if err := DecodeJSON(readFixture(t, file), d); err != nil {
				t.Fatal(err)
			}
			if err := ValidateDeclaration(d); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func wantCode(t *testing.T, err error, code modelv1alpha1.RejectionCode) {
	t.Helper()
	ve, ok := err.(*ValidationError) //nolint:errorlint // validators return it unwrapped
	if !ok || !ve.Has(code) {
		t.Fatalf("got %v, want %s", err, ShortName(code))
	}
}

func TestValidateDeclarationRejects(t *testing.T) {
	kind := func(k *modelv1alpha1.KindDeclaration) *modelv1alpha1.AdapterDeclaration {
		return &modelv1alpha1.AdapterDeclaration{Name: "x", IssuerType: "x", Kinds: []*modelv1alpha1.KindDeclaration{k}}
	}
	id := &modelv1alpha1.KeyTypeDeclaration{KeyType: "id", Class: modelv1alpha1.KeyClass_KEY_CLASS_ID}
	field := func(f *modelv1alpha1.FieldDeclaration) *modelv1alpha1.AdapterDeclaration {
		return kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{id}, Fields: []*modelv1alpha1.FieldDeclaration{f}})
	}
	tests := []struct {
		name string
		d    *modelv1alpha1.AdapterDeclaration
		want modelv1alpha1.RejectionCode
	}{
		{"no name", &modelv1alpha1.AdapterDeclaration{IssuerType: "x"}, codeMalformed},
		{"bad issuer", &modelv1alpha1.AdapterDeclaration{Name: "x", IssuerType: "X Y"}, codeMalformed},
		{"unknown kind", kind(&modelv1alpha1.KindDeclaration{Kind: "Widget"}), codeNotDeclared},
		{"no class", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{{KeyType: "t"}}}), codeMalformed},
		{"id redirects", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{{KeyType: "t", Class: modelv1alpha1.KeyClass_KEY_CLASS_ID, Redirects: true}}}), codeMalformed},
		{"key twice", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Keys: []*modelv1alpha1.KeyTypeDeclaration{id, id}}), codeMalformed},
		{"core predicate", field(&modelv1alpha1.FieldDeclaration{Predicate: "same_as"}), codeCorePredicate},
		{"domain", field(&modelv1alpha1.FieldDeclaration{Predicate: "approves_changes"}), codeDomainMismatch},
		{"range", field(&modelv1alpha1.FieldDeclaration{Predicate: "changed_by", Direction: modelv1alpha1.Direction_DIRECTION_IN}), codeDomainMismatch},
		{"attribute domain", field(&modelv1alpha1.FieldDeclaration{Predicate: "login"}), codeDomainMismatch},
		{"unregistered without type", field(&modelv1alpha1.FieldDeclaration{Predicate: "size", Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE}), codeMalformed},
		{"registered with other type", field(&modelv1alpha1.FieldDeclaration{Predicate: "name", Type: modelv1alpha1.ValueType_VALUE_TYPE_BOOL}), codeTypeMismatch},
		{"attribute in", field(&modelv1alpha1.FieldDeclaration{Predicate: "name", Direction: modelv1alpha1.Direction_DIRECTION_IN}), codeMalformed},
		{"members out", field(&modelv1alpha1.FieldDeclaration{Predicate: "member_of", Match: modelv1alpha1.MatchMethod_MATCH_METHOD_MEMBERS}), codeMalformed},
		{"exists", field(&modelv1alpha1.FieldDeclaration{Predicate: "exists"}), codeMalformed},
		{"bad link", kind(&modelv1alpha1.KindDeclaration{Kind: "Team", Links: []*modelv1alpha1.LinkDeclaration{{IssuerType: "saml"}}}), codeMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantCode(t, ValidateDeclaration(tt.d), tt.want)
		})
	}
}

func TestValidateManualEvent(t *testing.T) {
	const a, b = "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f"
	actor := &eventv1alpha1.Actor{Subject: "oidc|jdoe"}
	ok := []proto.Message{
		&eventv1alpha1.MergeRequested{Actor: actor, Reason: "same team", SubjectIds: []string{a, b}},
		&eventv1alpha1.UnmergeRequested{Actor: actor, Reason: "wrong merge", SubjectId: a, Aliases: []string{"github:user/jdoe"}},
		&eventv1alpha1.DistinctFromSet{Actor: actor, Reason: "r", SubjectIds: []string{a, b}},
		&eventv1alpha1.DistinctFromCleared{Actor: actor, Reason: "r", SubjectIds: []string{a, b}},
		&eventv1alpha1.ClaimWithdrawn{
			Actor: actor, Reason: "r", Source: "github-acme", SubjectId: a, Predicate: "owned_by",
			Object: &modelv1alpha1.FactObject{SubjectId: b},
		},
		&eventv1alpha1.OverrideSet{
			Actor: actor, Reason: "r", SubjectId: a, Predicate: "github.codeowners_rules",
			Objects: []*modelv1alpha1.FactObject{{Type: modelv1alpha1.ValueType_VALUE_TYPE_FLOAT, Value: structpb.NewNumberValue(1)}},
		},
		&eventv1alpha1.OverrideCleared{Actor: actor, Reason: "r", SubjectId: a, Predicate: "owned_by"},
	}
	for _, m := range ok {
		if err := ValidateManualEvent(m); err != nil {
			t.Errorf("%T: %v", m, err)
		}
	}

	at := timestamppb.Now()
	bad := []struct {
		m    proto.Message
		want modelv1alpha1.RejectionCode
	}{
		{&eventv1alpha1.MergeRequested{Reason: "r", SubjectIds: []string{a, b}}, codeMalformed},
		{&eventv1alpha1.MergeRequested{Actor: actor, SubjectIds: []string{a, b}}, codeMalformed},
		{&eventv1alpha1.MergeRequested{Actor: actor, Reason: "r", SubjectIds: []string{a}}, codeMalformed},
		{&eventv1alpha1.MergeRequested{Actor: actor, Reason: "r", SubjectIds: []string{a, a}}, codeInvalidOperation},
		{&eventv1alpha1.DistinctFromSet{Actor: actor, Reason: "r", SubjectIds: []string{a, "0192B1C4-5E11-7B4D-8E3F-7A2B3C4D5E6F"}}, codeMalformed},
		{&eventv1alpha1.UnmergeRequested{Actor: actor, Reason: "r", SubjectId: a}, codeInvalidOperation},
		{&eventv1alpha1.UnmergeRequested{Actor: actor, Reason: "r", SubjectId: a, Aliases: []string{"github:user/x", "github:user/x"}}, codeMalformed},
		{&eventv1alpha1.ClaimWithdrawn{Actor: actor, Reason: "r", SubjectId: a, Predicate: "owned_by", Object: &modelv1alpha1.FactObject{SubjectId: b}}, codeMalformed},
		{&eventv1alpha1.ClaimWithdrawn{Actor: actor, Reason: "r", Source: "s", SubjectId: a, Predicate: "owned_by"}, codeMalformed},
		{&eventv1alpha1.OverrideSet{Actor: actor, Reason: "r", SubjectId: a, Predicate: "Owned By"}, codeMalformed},
		{&eventv1alpha1.OverrideSet{
			Actor: actor, Reason: "r", SubjectId: a, Predicate: "owned_by",
			Objects: []*modelv1alpha1.FactObject{{SubjectId: b, Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING}},
		}, codeMalformed},
		{&eventv1alpha1.OverrideSet{
			Actor: actor, Reason: "r", SubjectId: a, Predicate: "name",
			Objects: []*modelv1alpha1.FactObject{{Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Value: structpb.NewBoolValue(true)}},
		}, codeTypeMismatch},
		{&eventv1alpha1.OverrideSet{Actor: actor, Reason: "r", SubjectId: a, Predicate: "name", ValidFrom: at, ValidTo: at}, codeInvalidInterval},
		{&eventv1alpha1.OverrideCleared{Actor: actor, Reason: "r", Predicate: "name"}, codeMalformed},
		{&eventv1alpha1.OverrideCleared{Actor: actor, Reason: "r", SubjectId: a, Predicate: "codeowners_rules"}, codeMalformed},
		{&eventv1alpha1.ClaimWithdrawn{
			Actor: actor, Reason: "r", Source: "s", SubjectId: a, Predicate: "owned_by",
			Object: &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType(42), Value: structpb.NewBoolValue(true)},
		}, codeMalformed},
		{&eventv1alpha1.SyncRequested{}, codeMalformed},
		{nil, codeMalformed},
	}
	for _, tt := range bad {
		wantCode(t, ValidateManualEvent(tt.m), tt.want)
	}
}
