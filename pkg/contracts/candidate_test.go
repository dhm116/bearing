package contracts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestCandidateJSONUsesProtoJSON(t *testing.T) {
	c := Candidate{
		Entity: &modelv1alpha1.Entity{
			Kind: "Team", Key: "github:team/acme/payments",
			Attributes: map[string]*structpb.Value{"name": structpb.NewStringValue("payments")},
		},
		Relations: []*modelv1alpha1.Relation{{
			Type: "owned_by", End: &modelv1alpha1.Relation_To{To: "github:repo/acme/api"},
			ValidFrom: timestamppb.New(time.Date(2026, 9, 28, 1, 30, 0, 0, time.UTC)),
		}},
		Span: "payments owns api",
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"to":"github:repo/acme/api"`, `"attributes":{"name":"payments"}`, `"valid_from":"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("got %s, want it to contain %s", b, want)
		}
	}
	var got Candidate
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Span != c.Span || !proto.Equal(got.Entity, c.Entity) || len(got.Relations) != 1 || !proto.Equal(got.Relations[0], c.Relations[0]) {
		t.Fatalf("got %+v, want %+v", got, c)
	}
}

func TestCandidateJSONRejectsBadMessages(t *testing.T) {
	for _, in := range []string{`{"entity":{"nope":1}}`, `{"relations":[{"type":7}]}`, `[]`} {
		var c Candidate
		if err := json.Unmarshal([]byte(in), &c); err == nil {
			t.Errorf("Unmarshal(%s): got nil error, want one", in)
		}
	}
	var empty Candidate
	b, err := json.Marshal(empty)
	if err != nil || string(b) != `{"entity":null,"span":""}` {
		t.Fatalf("got %s, %v, want an empty candidate", b, err)
	}
	if err := json.Unmarshal(b, &empty); err != nil || empty.Entity != nil {
		t.Fatalf("got %+v, %v, want no entity", empty, err)
	}
}
