package query_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/duckstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/query"
)

func runtimeVal(lang, version, file string) map[string]any {
	return map[string]any{"language": lang, "version": version, "file": file}
}

// newRuntimesQuerier holds repositories with declared versions: api (Go
// 1.21), svc (Go 1.23.1), both (go.mod 1.23 and .tool-versions golang 1.20),
// web (node >=18 and 20, go 1.22), odd (node lts/hydrogen), mixed (node
// lts/iron and 16) and a Service that declares go 1.19.
func newRuntimesQuerier(t *testing.T) *query.Querier {
	t.Helper()
	s, err := duckstore.Open(context.Background(), duckstore.Config{Now: func() time.Time { return at(100) }, NewID: testkit.NewSeqIDs("e").NewID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rts := func(vals ...map[string]any) func(*modelv1alpha1.ObservationData) {
		l := make([]any, len(vals))
		for i, v := range vals {
			l[i] = v
		}
		return attr("runtime", l)
	}
	obs := []*eventv1alpha1.Observation{
		ob("r1", 1, "Repository", "github:repo/acme/api", rts(runtimeVal("go", "1.21", "go.mod"))),
		ob("r2", 1, "Repository", "github:repo/acme/svc", rts(runtimeVal("go", "v1.23.1", "go.mod"))),
		ob("r3", 1, "Repository", "github:repo/acme/both", rts(runtimeVal("go", "1.23", "go.mod"), runtimeVal("golang", "1.20", ".tool-versions"))),
		ob("r4", 1, "Repository", "github:repo/acme/web", rts(runtimeVal("node", ">=18", "package.json"), runtimeVal("node", "20", ".nvmrc"), runtimeVal("go", "1.22", "go.mod"))),
		ob("r5", 1, "Repository", "github:repo/acme/odd", rts(runtimeVal("node", "lts/hydrogen", ".nvmrc"))),
		ob("r6", 1, "Repository", "github:repo/acme/mixed", rts(runtimeVal("nodejs", "lts/iron", ".tool-versions"), runtimeVal("node", "16", ".nvmrc"))),
		ob("r7", 1, "Service", "kubernetes:service/acme/job", rts(runtimeVal("go", "1.19", "go.mod"))),
	}
	if _, err := s.Ingest(context.Background(), graph.IngestRequest{Source: "github-acme", Observations: obs}); err != nil {
		t.Fatal(err)
	}
	return &query.Querier{Graph: s}
}

// names lists the keys of the entities in a group.
func names(es []query.RuntimeEntity) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Subject.Key)
	}
	return out
}

func TestRuntimesGroupsEntitiesByTheirLowestDeclaration(t *testing.T) {
	t.Parallel()
	q := newRuntimesQuerier(t)
	tests := []struct {
		name           string
		req            query.RuntimesRequest
		below, unknown []string
		atOrAbove      int
	}{
		{
			"go below 1.22",
			query.RuntimesRequest{Language: "go", Below: "1.22"},
			[]string{"github:repo/acme/api", "github:repo/acme/both", "kubernetes:service/acme/job"},
			nil, 2,
		},
		{
			"alias and case",
			query.RuntimesRequest{Language: "Golang", Below: "1.22", Kind: "Repository"},
			[]string{"github:repo/acme/api", "github:repo/acme/both"},
			nil, 2,
		},
		{
			"node below 20",
			query.RuntimesRequest{Language: "nodejs", Below: "20"},
			[]string{"github:repo/acme/mixed", "github:repo/acme/web"},
			[]string{"github:repo/acme/odd"},
			0,
		},
		{
			"node below 16 leaves the unknown",
			query.RuntimesRequest{Language: "node", Below: "16"},
			nil,
			[]string{"github:repo/acme/mixed", "github:repo/acme/odd"},
			1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := q.Runtimes(t.Context(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if b := names(got.Below); !slices.Equal(b, tc.below) {
				t.Errorf("below %v, want %v", b, tc.below)
			}
			if u := names(got.Unknown); !slices.Equal(u, tc.unknown) {
				t.Errorf("unknown %v, want %v", u, tc.unknown)
			}
			if got.AtOrAbove != tc.atOrAbove {
				t.Errorf("at or above %d, want %d", got.AtOrAbove, tc.atOrAbove)
			}
		})
	}
}

func TestRuntimesShowsEveryDeclarationOfAnEntityThatIsBelow(t *testing.T) {
	t.Parallel()
	got, err := newRuntimesQuerier(t).Runtimes(t.Context(), query.RuntimesRequest{Language: "go", Below: "1.22", Kind: "Repository"})
	if err != nil {
		t.Fatal(err)
	}
	both := got.Below[1]
	if both.Subject.Key != "github:repo/acme/both" || len(both.Declarations) != 2 {
		t.Fatalf("got %+v, want both with its two declarations", both)
	}
	d := both.Declarations[0] // sorted by file: .tool-versions first
	if d.File != ".tool-versions" || d.Version != "1.20" || d.Lower != "1.20" || !d.Below || len(d.Sources) == 0 || d.Sources[0].Source != "github-acme" {
		t.Errorf("first declaration %+v, want the .tool-versions 1.20, below, with its source", d)
	}
	if d := both.Declarations[1]; d.File != "go.mod" || d.Below {
		t.Errorf("second declaration %+v, want go.mod 1.23, not below", d)
	}
}

func TestRuntimesInventoryListsEveryDeclaration(t *testing.T) {
	t.Parallel()
	got, err := newRuntimesQuerier(t).Runtimes(t.Context(), query.RuntimesRequest{Language: "node"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Language != "node" || got.Below != nil || got.Unknown != nil {
		t.Fatalf("got %+v, want only entities", got)
	}
	if want := []string{"github:repo/acme/mixed", "github:repo/acme/odd", "github:repo/acme/web"}; !slices.Equal(names(got.Entities), want) {
		t.Fatalf("entities %v, want %v", names(got.Entities), want)
	}
	if d := got.Entities[1].Declarations[0]; !d.Unknown || d.Lower != "" || d.Version != "lts/hydrogen" {
		t.Errorf("odd's declaration %+v, want unknown", d)
	}
}

func TestRuntimesLimitCutsEachList(t *testing.T) {
	t.Parallel()
	got, err := newRuntimesQuerier(t).Runtimes(t.Context(), query.RuntimesRequest{Language: "go", Below: "2", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Below) != 2 || !got.Truncated {
		t.Fatalf("got %d below, truncated %v, want 2 and true", len(got.Below), got.Truncated)
	}
}

func TestRuntimesRefusesMalformedRequests(t *testing.T) {
	t.Parallel()
	q := newRuntimesQuerier(t)
	for name, req := range map[string]query.RuntimesRequest{
		"no language":         {},
		"below not a version": {Language: "go", Below: "lts/hydrogen"},
		"limit too large":     {Language: "go", Limit: query.MaxRuntimesLimit + 1},
		"kind not a kind":     {Language: "go", Kind: "not a kind"},
	} {
		if _, err := q.Runtimes(t.Context(), req); !errors.Is(err, query.ErrInvalidRequest) {
			t.Errorf("%s: got %v, want ErrInvalidRequest", name, err)
		}
	}
}
