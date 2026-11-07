package duckstore

import (
	"errors"
	"slices"
	"testing"
	"time"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// hierarchyStore is a store whose vocabulary has k8s.Service and
// systemd.Unit under Component, and acme.Api under k8s.Service.
func hierarchyStore(t *testing.T) *Store {
	t.Helper()
	reg, err := model.Core().Extend([]model.KindDef{
		{Name: "k8s.Service", Parent: model.KindComponent},
		{Name: "systemd.Unit", Parent: model.KindComponent},
		{Name: "acme.Api", Parent: "k8s.Service"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newStore(t, Config{Now: func() time.Time { return at(1000) }, NewID: testkit.NewSeqIDs("id-").NewID, Registry: reg})
}

// idOf is the record a key names.
func idOf(t *testing.T, s *Store, key string) string {
	t.Helper()
	return string(clusterOf(t, s, key))
}

func TestLinkClustersKindsOnOneLineOfDescentAndSkipsOneOffIt(t *testing.T) {
	t.Parallel()
	s := hierarchyStore(t)
	ingestOK(t, s, "src",
		obs("o1", 0, "Component", "c:a/1"),
		obs("o2", 1, "k8s.Service", "k:a/1"),
		obs("o3", 2, "systemd.Unit", "u:a/1"),
		obs("o4", 3, "acme.Api", "a:a/1"))
	c, k, u, a := idOf(t, s, "c:a/1"), idOf(t, s, "k:a/1"), idOf(t, s, "u:a/1"), idOf(t, s, "a:a/1")

	link(t, s, sameAs(c, k, graph.Asserted, "r"))
	wantClusters(t, s, map[string]graph.EntityID{"c:a/1": graph.EntityID(c), "k:a/1": graph.EntityID(c)})

	// The cluster is now a k8s.Service line: a systemd.Unit is off it, an acme.Api below it.
	link(t, s, sameAs(k, u, graph.Asserted, "r"), sameAs(k, a, graph.Asserted, "r"))
	wantClusters(t, s, map[string]graph.EntityID{
		"c:a/1": graph.EntityID(c), "k:a/1": graph.EntityID(c), "a:a/1": graph.EntityID(c), "u:a/1": graph.EntityID(u),
	})
}

func TestDecideJoinRefusesAClusterOffTheLineButJoinsAParentAndChild(t *testing.T) {
	t.Parallel()
	s := hierarchyStore(t)
	ingestOK(t, s, "src",
		obs("o1", 0, "Component", "c:a/1"),
		obs("o2", 1, "k8s.Service", "k:a/1"),
		obs("o3", 2, "systemd.Unit", "u:a/1"))
	c, k, u := idOf(t, s, "c:a/1"), idOf(t, s, "k:a/1"), idOf(t, s, "u:a/1")

	decide(t, s, graph.Decision{ID: "e1", Kind: graph.Join, A: c, B: k})
	_, err := s.Decide(t.Context(), graph.Decision{ID: "e2", Kind: graph.Join, A: k, B: u, Actor: "local:501"})
	if !errors.Is(err, graph.ErrInvalidDecision) {
		t.Fatalf("got %v, want ErrInvalidDecision for a systemd.Unit joining a k8s.Service cluster", err)
	}
	wantClusters(t, s, map[string]graph.EntityID{"c:a/1": graph.EntityID(c), "k:a/1": graph.EntityID(c), "u:a/1": graph.EntityID(u)})
}

func TestRecordsAndFindOfAKindIncludeItsDescendants(t *testing.T) {
	t.Parallel()
	s := hierarchyStore(t)
	ingestOK(t, s, "src",
		obs1("c", 0, "Component", "c:svc/checkout"),
		obs1("k", 1, "k8s.Service", "k:svc/checkout-k8s"),
		obs1("a", 2, "acme.Api", "a:svc/checkout-api"),
		obs1("u", 3, "systemd.Unit", "u:svc/checkout-unit"),
		obs1("t", 4, "Team", "t:team/checkout"))
	kinds := func(rs []graph.Record) []model.Kind {
		var out []model.Kind
		for _, r := range rs {
			out = append(out, r.Kind)
		}
		slices.Sort(out)
		return out
	}
	tests := []struct {
		kind model.Kind
		want []model.Kind
	}{
		{"Component", []model.Kind{"Component", "acme.Api", "k8s.Service", "systemd.Unit"}},
		{"k8s.Service", []model.Kind{"acme.Api", "k8s.Service"}},
		{"acme.Api", []model.Kind{"acme.Api"}},
		{"Team", []model.Kind{"Team"}},
	}
	for _, tc := range tests {
		got, err := s.Records(t.Context(), tc.kind)
		if err != nil || !slices.Equal(kinds(got), tc.want) {
			t.Errorf("Records(%s) got %v, %v, want kinds %v", tc.kind, kinds(got), err, tc.want)
		}
	}
	ms, err := s.Find(t.Context(), "checkout", "Component", 20)
	if err != nil || len(ms) != 4 {
		t.Errorf("Find in Component got %d matches, %v, want the 4 Component-line records", len(ms), err)
	}
	if ms, err = s.Find(t.Context(), "checkout", "k8s.Service", 20); err != nil || len(ms) != 2 {
		t.Errorf("Find in k8s.Service got %d matches, %v, want 2", len(ms), err)
	}
}

func TestIngestCompleteSyncOfAKindClosesItsDescendantsToo(t *testing.T) {
	t.Parallel()
	s := hierarchyStore(t)
	ingestOK(t, s, "k8s",
		obs("o1", 0, "k8s.Service", "k:svc/gone", attrs("tier", "1")),
		obs("o2", 0, "acme.Api", "a:svc/gone", attrs("tier", "2")),
		obs("o3", 0, "systemd.Unit", "u:svc/sibling", attrs("tier", "3")))
	// k8s.Service and its child close; the sibling branch stays open.
	r := ingestReq(t, s, graph.IngestRequest{Source: "k8s", Complete: []model.Kind{"k8s.Service"}, SyncStarted: at(10)})
	if r.Closed != 2 {
		t.Fatalf("got %+v, want the k8s.Service and acme.Api facts closed", r)
	}
	// Component covers every kind below it, the sibling included.
	r = ingestReq(t, s, graph.IngestRequest{Source: "k8s", Complete: []model.Kind{"Component"}, SyncStarted: at(20)})
	if r.Closed != 1 {
		t.Fatalf("got %+v, want the systemd.Unit fact closed by a complete Component sync", r)
	}
}
