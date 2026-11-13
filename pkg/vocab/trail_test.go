package vocab

import "testing"

// TestTrailJoinNeedsAWatchClaim: a monitor whose configuration only lives in
// a service's repository is not taken to watch it.
func TestTrailJoinNeedsAWatchClaim(t *testing.T) {
	t.Parallel()
	tr := Trail{Watches: "watches", Pages: "pages", Describes: "describes", DefinedIn: "defined_in"}
	edges := []Edge{
		{Fact: "f1", Subject: "tf", Object: "mon", Predicate: "describes", ObjectObserved: true},
		{Fact: "f2", Subject: "tf", Object: "repo", Predicate: "defined_in"},
		{Fact: "f3", Subject: "mon", Object: "pd", Predicate: "pages", ObjectObserved: true},
	}
	if cs, bs := tr.Join(edges); len(cs) != 0 || len(bs) != 0 {
		t.Fatalf("join = %+v, %+v; want nothing", cs, bs)
	}
	edges = append(edges, Edge{Fact: "f4", Subject: "mon", Object: "repo", Predicate: "watches"})
	cs, _ := tr.Join(edges)
	if len(cs) != 1 || len(cs[0].Reach) != 2 || cs[0].Score != 2.0/3*0.5 {
		t.Fatalf("join = %+v; want one trail with two reach claims at 1/3", cs)
	}
	if got := cs[0].Facts(); len(got) != 4 {
		t.Errorf("path = %v, want all four facts", got)
	}
}
