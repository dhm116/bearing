package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
)

// retiredQuerier holds an acme.Api (below k8s.Service, below Component) seen
// at minute 1, then boots again without acme.Api at minute 50.
func retiredQuerier(t *testing.T) *query.Querier {
	t.Helper()
	full, err := model.Core().Extend([]model.KindDef{
		{Name: "k8s.Service", Parent: model.KindComponent},
		{Name: "acme.Api", Parent: "k8s.Service"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := duckstore.Open(context.Background(), duckstore.Config{
		Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID, Registry: full,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if _, err := s.SyncVocabulary(ctx, full, at(0)); err != nil {
		t.Fatal(err)
	}
	obs := []*eventv1alpha1.Observation{
		ob("s1", 1, "acme.Api", "kubernetes:service/acme/checkout-api", attr("name", "checkout api"), attr("language", "Go"), attr("runtime", []any{runtimeVal("go", "1.19", "go.mod")})),
		ob("s2", 1, "Component", "kubernetes:service/acme/job", attr("name", "checkout job"), attr("language", "Go"), attr("runtime", []any{runtimeVal("go", "1.19", "go.mod")})),
	}
	if _, err := s.Ingest(ctx, graph.IngestRequest{Source: "k8s", Observations: obs}); err != nil {
		t.Fatal(err)
	}
	dropped, err := model.Core().Extend([]model.KindDef{{Name: "k8s.Service", Parent: model.KindComponent}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.SyncVocabulary(ctx, dropped, at(50))
	if err != nil {
		t.Fatal(err)
	}
	s.SetRegistry(read)
	return &query.Querier{Graph: s, Registry: read}
}

func TestGetLabelsFactsOfARetiredKindAsOfItsRetirement(t *testing.T) {
	t.Parallel()
	q := retiredQuerier(t)
	tests := []struct {
		ref  string
		want *time.Time
	}{
		{"kubernetes:service/acme/checkout-api", ptr(at(50))},
		{"kubernetes:service/acme/job", nil},
	}
	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			e, err := q.Get(t.Context(), tc.ref, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(e.Facts) == 0 {
				t.Fatal("got no facts")
			}
			for _, f := range e.Facts {
				if (f.AsOf == nil) != (tc.want == nil) || f.AsOf != nil && !f.AsOf.Equal(*tc.want) {
					t.Errorf("got %s as_of %v, want %v", f.Predicate, f.AsOf, tc.want)
				}
			}
		})
	}
}

func TestGetBeforeTheRetirementLeavesFactsUnlabeled(t *testing.T) {
	t.Parallel()
	q := retiredQuerier(t)
	e, err := q.Get(t.Context(), "kubernetes:service/acme/checkout-api", at(10))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range e.Facts {
		if f.AsOf != nil {
			t.Errorf("got %s as_of %v at a time before the retirement, want none", f.Predicate, f.AsOf)
		}
	}
}

func TestChangesUseFactsOfARetiredKindUnchanged(t *testing.T) {
	t.Parallel()
	q := retiredQuerier(t)
	cs, err := q.Changes(t.Context(), "", at(0), at(60), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Changes) == 0 {
		t.Fatal("got no changes")
	}
	for _, c := range cs.Changes {
		if c.Fact.AsOf != nil {
			t.Errorf("got %s as_of %v in a history answer, want none", c.Fact.Predicate, c.Fact.AsOf)
		}
	}
}

func TestFindAcceptsARetiredKindAndAncestorsStillCountItsFactsButNeverHeldKindsFail(t *testing.T) {
	t.Parallel()
	q := retiredQuerier(t)
	tests := []struct {
		kind    string
		want    int
		invalid bool
	}{
		{"acme.Api", 1, false},
		{"k8s.Service", 1, false},
		{"Component", 2, false},
		{"acme.Never", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			ms, err := q.Find(t.Context(), "checkout", tc.kind, 0)
			if tc.invalid != errors.Is(err, query.ErrInvalidRequest) || !tc.invalid && err != nil {
				t.Fatalf("got error %v, want invalid=%v", err, tc.invalid)
			}
			if len(ms) != tc.want {
				t.Errorf("got %d matches, want %d", len(ms), tc.want)
			}
		})
	}
}

func TestRuntimesLabelsDeclarationsOfARetiredKindAsOfItsRetirement(t *testing.T) {
	t.Parallel()
	q := retiredQuerier(t)
	r, err := q.Runtimes(t.Context(), query.RuntimesRequest{Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Entities) != 2 {
		t.Fatalf("got %d entities, want 2", len(r.Entities))
	}
	for _, e := range r.Entities {
		wantRetired := e.Subject.Kind == "acme.Api"
		for _, d := range e.Declarations {
			if (d.AsOf != nil) != wantRetired || wantRetired && !d.AsOf.Equal(at(50)) {
				t.Errorf("got %s declaration as_of %v, want retired=%v at %v", e.Subject.Key, d.AsOf, wantRetired, at(50))
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }
