package match

import (
	"testing"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

func rec(id, name string, kind model.Kind, keys ...model.Key) graph.Record {
	return graph.Record{ID: graph.EntityID(id), Cluster: graph.EntityID(id), Kind: kind, Name: name, Keys: keys}
}

func TestBlockFindsCandidatePairs(t *testing.T) {
	members := map[graph.EntityID]map[graph.EntityID]bool{
		"g-sre": {"p1": true, "p2": true},
		"k-rel": {"p2": true},
	}
	recs := []graph.Record{
		rec("g-pay", "payments", model.KindTeam),
		rec("d-pay", "Payments", model.KindTeam),
		rec("t-pay", "payments-team", model.KindTeam),
		rec("g-roll", "payroll", model.KindTeam),
		rec("g-sre", "sre", model.KindTeam),
		rec("k-rel", "reliability", model.KindTeam),
		rec("person", "payments", model.KindPerson),
	}
	recs[2].Cluster = "g-pay" // already joined to g-pay
	skip := func(a, b graph.EntityID) bool { return a == "d-pay" && b == "g-roll" }

	var got []string
	for _, c := range Block(recs, members, skip) {
		got = append(got, string(c.A.ID)+"+"+string(c.B.ID))
	}
	want := []string{"d-pay+g-pay", "g-sre+k-rel", "d-pay+t-pay"}
	// sort-independent comparison
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing %s in %v", w, got)
		}
	}
}

func TestBlockSkipsSameClusterAndOtherKinds(t *testing.T) {
	a, b := rec("a", "payments", model.KindTeam), rec("b", "payments", model.KindTeam)
	b.Cluster = "a"
	if got := Block([]graph.Record{a, b}, nil, nil); len(got) != 0 {
		t.Fatalf("same cluster: got %v", got)
	}
	c := rec("c", "payments", model.KindPerson)
	if got := Block([]graph.Record{a, c}, nil, nil); len(got) != 0 {
		t.Fatalf("other kind: got %v", got)
	}
}
