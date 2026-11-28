package query

import (
	"slices"
	"strings"
	"testing"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// TestClaimsFollowTheOrderRules feeds claims in the reverse of ADR 11's
// order, each pair differing only by the rule under test, and wants them
// sorted: fewer facts and hops, then the smallest source name, the object's
// first key, the subject's first key and the fact ID.
func TestClaimsFollowTheOrderRules(t *testing.T) {
	t.Parallel()
	recs := map[graph.EntityID]graph.Record{}
	for _, r := range []struct{ id, key string }{{"s1", "k:s/1"}, {"s2", "k:s/2"}, {"o1", "k:o/1"}, {"o2", "k:o/2"}} {
		recs[graph.EntityID(r.id)] = graph.Record{ID: graph.EntityID(r.id), Cluster: graph.EntityID(r.id), Kind: model.Kind("Team"), Keys: []model.Key{model.Key(r.key)}}
	}
	claim := func(id, source, subject, object string, facts, hops int) graph.Claim {
		f := graph.Fact{ID: id, Subject: graph.EntityID(subject), Object: graph.EntityID(object), Status: graph.Asserted, Sources: []graph.Source{{Source: source}}}
		return graph.Claim{Fact: f, Start: graph.EntityID(subject), Facts: facts, SameAs: hops}
	}
	// Each claim would come first by the next rule down, so dropping a rule
	// shows; the input is reversed, so a stable sort keeps nothing in place.
	ev := graph.Evidence{Claims: []graph.Claim{
		claim("f8", "a", "s1", "o1", 2, 0), // rule 1: more facts, though no hop
		claim("f7", "a", "s1", "o1", 1, 1), // rule 1: a hop, though a smaller source
		claim("f6", "z", "s1", "o1", 1, 0), // rule 2: a later source, though a smaller object key
		claim("f0", "b", "s1", "o2", 1, 0), // rule 3: a later object key, though a smaller subject key
		claim("f1", "b", "s2", "o1", 1, 0), // rule 4: a later subject key, though a smaller fact ID
		claim("f5", "b", "s1", "o1", 1, 0), // rule 5: a later fact ID
		claim("f4", "b", "s1", "o1", 1, 0),
	}}
	subs := []Subject{{Ref: Ref{ID: "s1"}}, {Ref: Ref{ID: "s2"}}}
	got, err := claims((&Querier{}).labeler(), ev, subs, recs)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.Fact.ID)
	}
	if want := []string{"f4", "f5", "f1", "f0", "f6", "f7", "f8"}; !slices.Equal(ids, want) {
		t.Errorf("got order %v, want %v", ids, want)
	}
}

// TestClaimsRefuseARecordTheStoreDidNotReturn: every record on a path comes
// with the evidence, so a missing one is a store bug, reported, not named
// by a bare ID.
func TestClaimsRefuseARecordTheStoreDidNotReturn(t *testing.T) {
	t.Parallel()
	recs := map[graph.EntityID]graph.Record{"s1": {ID: "s1", Cluster: "s1"}}
	f := graph.Fact{ID: "f1", Subject: "s1", Object: "gone", Status: graph.Asserted}
	for name, c := range map[string]graph.Claim{
		"fact object": {Fact: f, Start: "s1"},
		"path step":   {Fact: graph.Fact{ID: "f2", Subject: "s1", Object: "s1"}, Start: "s1", Path: []graph.PathStep{{From: "s1", To: "gone"}}},
	} {
		_, err := claims((&Querier{}).labeler(), graph.Evidence{Claims: []graph.Claim{c}}, []Subject{{Ref: Ref{ID: "s1"}}}, recs)
		if err == nil || !strings.Contains(err.Error(), "record gone") {
			t.Errorf("%s: got %v, want an error naming the missing record", name, err)
		}
	}
	if _, err := claims((&Querier{}).labeler(), graph.Evidence{Claims: []graph.Claim{{Fact: f, Start: "other"}}}, []Subject{{Ref: Ref{ID: "s1"}}}, recs); err == nil {
		t.Error("a claim starting at no subject: got no error")
	}
}
