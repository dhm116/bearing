package duckstore

import (
	"slices"
	"testing"

	"bearing.example/pkg/graph"
)

func TestLinksWindowIsHalfOpenAndSeesOpeningsAndClosings(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1", "p:c/1")
	open := func(a, b string, minute int) string {
		p := sameAs(a, b, graph.Asserted, "rule/x")
		p.Deposit.At = at(minute)
		link(t, s, p)
		return rowOf(t, s, graph.SameAs, a, b).ID
	}
	ab := open("id-1", "id-2", 10) // opens at t10, closes at t40
	open("id-1", "id-3", 20)       // opens at t20
	if err := s.CloseLink(t.Context(), ab, at(40)); err != nil {
		t.Fatal(err)
	}
	pairs := func(f graph.LinkFilter) []string {
		var out []string
		for _, l := range links(t, s, f) {
			out = append(out, string(l.A)+string(l.B))
		}
		slices.Sort(out)
		return out
	}
	tests := []struct {
		name string
		f    graph.LinkFilter
		want []string
	}{
		{"a row opened exactly at Since is included", graph.LinkFilter{ChangedSince: at(10), ChangedUntil: at(15)}, []string{"id-1id-2"}},
		{"a row opened exactly at Until is excluded", graph.LinkFilter{ChangedSince: at(5), ChangedUntil: at(10)}, nil},
		{"a row caught only by its valid_to", graph.LinkFilter{ChangedSince: at(35), ChangedUntil: at(45)}, []string{"id-1id-2"}},
		{"Until alone", graph.LinkFilter{ChangedUntil: at(20)}, []string{"id-1id-2"}},
		{"Since alone", graph.LinkFilter{ChangedSince: at(20)}, []string{"id-1id-2", "id-1id-3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pairs(tc.f); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLinkPromotionOpensTheAssertedRowWhenTheProposalClosesNotEarlier(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	proposal := sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent")
	proposal.Deposit.At = at(10)
	link(t, s, proposal)
	promoting := sameAs("id-1", "id-2", graph.Asserted, "rule/teams")
	promoting.Deposit.At = at(5) // older than the proposal
	link(t, s, promoting)

	closed := links(t, s, graph.LinkFilter{Status: graph.Proposed, WithClosed: true})[0]
	asserted := links(t, s, graph.LinkFilter{Status: graph.Asserted})[0]
	if !closed.ValidTo.Equal(at(10)) || !asserted.ValidFrom.Equal(at(10)) || !asserted.Deposits[0].At.Equal(at(5)) {
		t.Errorf("got proposal closed %v, asserted from %v, deposit %v; want both rows at t10 and the deposit kept at t5",
			closed.ValidTo, asserted.ValidFrom, asserted.Deposits[0].At)
	}
}

func TestIngestPromotesAProposedSharedKeyLinkWhenALaterObservationAssertsIt(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	// bs/b1 names gh/x only as an alias: a proposed shared_key link to gh's record.
	ingestOK(t, s, "gh", obs("o1", 0, "Component", "gh:repo/x", aliases("gh:repo/x2")))
	ingestOK(t, s, "bs", obs("o2", 1, "Component", "bs:comp/x", aliases("gh:repo/x2")))
	proposed := links(t, s, graph.LinkFilter{Kind: graph.SameAs, Status: graph.Proposed})
	if len(proposed) != 1 || proposed[0].Rule != "shared_key" {
		t.Fatalf("got %+v, want one proposed shared_key link", proposed)
	}
	// A later observation of the key as a primary key asserts the link.
	ingestOK(t, s, "gh", obs("o3", 5, "Component", "gh:repo/x2"))
	ingestOK(t, s, "bs", obs("o4", 6, "Component", "bs:comp/x", aliases("gh:repo/x2")))
	asserted := links(t, s, graph.LinkFilter{Kind: graph.SameAs, Status: graph.Asserted})
	if len(asserted) != 1 || asserted[0].PromotedFrom != proposed[0].ID || asserted[0].Rule != "shared_key" ||
		asserted[0].OpenEffect != graph.EffectJoined || asserted[0].OpenDecision != "" || len(asserted[0].Deposits) != 1 ||
		asserted[0].Deposits[0].ObservationID != "o4" {
		t.Errorf("got %+v, want an asserted shared_key link promoted from %s by o4", asserted, proposed[0].ID)
	}
	old := links(t, s, graph.LinkFilter{ID: proposed[0].ID, WithClosed: true})[0]
	if old.ValidTo.IsZero() {
		t.Errorf("got %+v, want the proposal closed", old)
	}
}
