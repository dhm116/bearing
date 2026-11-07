package query_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
)

// newServiceQuerier holds a Component, a k8s.Service and an acme.Api (below
// k8s.Service) that each declare Go 1.19, and a Team, under a registry where
// k8s.Service extends Component.
func newServiceQuerier(t *testing.T) *query.Querier {
	t.Helper()
	reg, err := model.Core().Extend([]model.KindDef{
		{Name: "k8s.Service", Parent: model.KindComponent},
		{Name: "acme.Api", Parent: "k8s.Service"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := duckstore.Open(context.Background(), duckstore.Config{
		Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID, Registry: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	old := attr("runtime", []any{runtimeVal("go", "1.19", "go.mod")})
	obs := []*eventv1alpha1.Observation{
		ob("s1", 1, "Component", "kubernetes:service/acme/job", attr("name", "checkout job"), old),
		ob("s2", 1, "k8s.Service", "kubernetes:service/acme/checkout", attr("name", "checkout"), old),
		ob("s3", 1, "acme.Api", "kubernetes:service/acme/checkout-api", attr("name", "checkout api"), old),
		ob("s4", 1, "Team", "github:team/acme/checkout", attr("name", "checkout")),
	}
	if _, err := s.Ingest(context.Background(), graph.IngestRequest{Source: "k8s", Observations: obs}); err != nil {
		t.Fatal(err)
	}
	return &query.Querier{Graph: s, Registry: reg}
}

func TestFindByAKindIncludesItsDescendants(t *testing.T) {
	t.Parallel()
	q := newServiceQuerier(t)
	tests := []struct {
		kind string
		want []string
	}{
		{"Component", []string{"kubernetes:service/acme/checkout", "kubernetes:service/acme/checkout-api", "kubernetes:service/acme/job"}},
		{"k8s.Service", []string{"kubernetes:service/acme/checkout", "kubernetes:service/acme/checkout-api"}},
		{"acme.Api", []string{"kubernetes:service/acme/checkout-api"}},
	}
	for _, tc := range tests {
		ms, err := q.Find(t.Context(), "checkout", tc.kind, 0)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range ms {
			got = append(got, m.Key)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("Find in %s got %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestRuntimesByAKindIncludeItsDescendants(t *testing.T) {
	t.Parallel()
	q := newServiceQuerier(t)
	got, err := q.Runtimes(t.Context(), query.RuntimesRequest{Language: "go", Below: "1.22", Kind: "Component"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubernetes:service/acme/checkout", "kubernetes:service/acme/checkout-api", "kubernetes:service/acme/job"}
	if g := names(got.Below); !slices.Equal(sorted(g), want) {
		t.Errorf("got %v below, want every Component-line record %v", g, want)
	}
}

func TestQueriesRefuseAKindOnlyAConfiguredRegistryKnows(t *testing.T) {
	t.Parallel()
	_, err := (&query.Querier{Graph: newServiceQuerier(t).Graph}).Find(t.Context(), "x", "k8s.Service", 0)
	if !errors.Is(err, query.ErrInvalidRequest) {
		t.Fatalf("got %v, want the core registry to refuse k8s.Service", err)
	}
}

func sorted(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }
