package model

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestKeyParse(t *testing.T) {
	tests := []struct {
		key             Key
		system, typ, id string
		wantErr         bool
	}{
		{key: "github:user/jdoe", system: "github", typ: "user", id: "jdoe"},
		{key: "github:repo/acme/payments-api", system: "github", typ: "repo", id: "acme/payments-api"},
		{key: "aws:resource/arn:aws:s3:::bucket", system: "aws", typ: "resource", id: "arn:aws:s3:::bucket"},
		{key: "github:user", wantErr: true},
		{key: "GitHub:user/jdoe", wantErr: true},
		{key: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.key), func(t *testing.T) {
			system, typ, id, err := tt.key.Parse()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.key)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if system != tt.system || typ != tt.typ || id != tt.id {
				t.Fatalf("got %q %q %q", system, typ, id)
			}
		})
	}
}

func TestNewObservationValidates(t *testing.T) {
	o := NewObservation("adapter/test", time.Date(2026, 9, 28, 1, 30, 0, 0, time.UTC), ObservationData{
		Entity:    Entity{Kind: KindTeam, Key: "github:team/acme/payments"},
		Relations: []Relation{{Type: RelMemberOf, To: "github:team/acme/engineering"}},
	})
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	if o.ID != "github:team/acme/payments@2026-09-28T01:30:00Z" {
		t.Fatalf("unexpected id %q", o.ID)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	o := Observation{Data: ObservationData{
		Entity:    Entity{Kind: "Widget", Key: "nope"},
		Relations: []Relation{{Type: "likes", To: "also-nope"}},
	}}
	err := o.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"specversion", "id is required", "unknown entity kind", "invalid key", "relations[0]: unknown type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestExampleObservationsDecode(t *testing.T) {
	b, err := os.ReadFile("../../testdata/observations.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i, line := range lines {
		if _, err := DecodeObservation([]byte(line)); err != nil {
			t.Errorf("line %d: %v", i+1, err)
		}
	}
}

func TestSchemaListsEveryKindAndRelation(t *testing.T) {
	b, err := os.ReadFile("../../schema/observation.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	kinds := schema.Defs["kind"].Enum
	if len(kinds) != len(Kinds) {
		t.Fatalf("schema has %d kinds, Go has %d", len(kinds), len(Kinds))
	}
	for i, k := range Kinds {
		if kinds[i] != string(k) {
			t.Errorf("kind %d: schema %q, Go %q", i, kinds[i], k)
		}
	}
	rels := schema.Defs["relationType"].Enum
	if len(rels) != len(RelationTypes) {
		t.Fatalf("schema has %d relation types, Go has %d", len(rels), len(RelationTypes))
	}
	for i, r := range RelationTypes {
		if rels[i] != string(r) {
			t.Errorf("relation %d: schema %q, Go %q", i, rels[i], r)
		}
	}
}
