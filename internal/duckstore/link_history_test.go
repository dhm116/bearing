package duckstore

import (
	"errors"
	"testing"

	"bearing.example/pkg/graph"
)

func distinct(a, b string) graph.Proposal {
	return graph.Proposal{
		A: graph.EntityID(a), B: graph.EntityID(b), Kind: graph.DistinctFrom, Status: graph.Asserted,
		Rule: graph.PersonRule, Deposit: graph.Deposit{At: at(60), Weight: 1},
	}
}

// rowOf returns the one asserted link of a kind between a and b, open or closed.
func rowOf(t *testing.T, s *Store, kind graph.LinkKind, a, b string) graph.Link {
	t.Helper()
	var got []graph.Link
	for _, l := range links(t, s, graph.LinkFilter{Kind: kind, Status: graph.Asserted, WithClosed: true}) {
		if string(l.A) == a && string(l.B) == b {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d asserted %s links between %s and %s, want 1: %+v", len(got), kind, a, b, got)
	}
	return got[0]
}

func TestLinkRecordsWhatEachAssertedRowDidToTheClusters(t *testing.T) {
	t.Parallel()
	closeRow := func(kind graph.LinkKind, a, b string) func(*testing.T, *Store) {
		return func(t *testing.T, s *Store) {
			if err := s.CloseLink(t.Context(), rowOf(t, s, kind, a, b).ID, at(70)); err != nil {
				t.Fatal(err)
			}
		}
	}
	asserted := func(a, b string) graph.Proposal { return sameAs(a, b, graph.Asserted, "rule/x") }
	tests := []struct {
		name    string
		setup   []graph.Proposal
		act     func(*testing.T, *Store)
		kind    graph.LinkKind
		a, b    string
		closing bool
		want    graph.LinkEffect
	}{
		{
			name: "same_as opens apart to together", setup: []graph.Proposal{asserted("id-1", "id-2")},
			kind: graph.SameAs, a: "id-1", b: "id-2", want: graph.EffectJoined,
		},
		{
			name: "same_as opens when already together", setup: []graph.Proposal{asserted("id-1", "id-3"), asserted("id-2", "id-3"), asserted("id-1", "id-2")},
			kind: graph.SameAs, a: "id-1", b: "id-2", want: graph.EffectNone,
		},
		{
			name: "same_as opens across a distinct_from", setup: []graph.Proposal{distinct("id-1", "id-2"), asserted("id-1", "id-2")},
			kind: graph.SameAs, a: "id-1", b: "id-2", want: graph.EffectRefused,
		},
		{
			name: "same_as closes together to apart", setup: []graph.Proposal{asserted("id-1", "id-2")}, act: closeRow(graph.SameAs, "id-1", "id-2"),
			kind: graph.SameAs, a: "id-1", b: "id-2", closing: true, want: graph.EffectSplit,
		},
		{
			name: "same_as closes while another path holds them", setup: []graph.Proposal{asserted("id-1", "id-2"), asserted("id-1", "id-3"), asserted("id-2", "id-3")},
			act:  closeRow(graph.SameAs, "id-1", "id-2"),
			kind: graph.SameAs, a: "id-1", b: "id-2", closing: true, want: graph.EffectNone,
		},
		{
			name: "distinct_from opens together to apart", setup: []graph.Proposal{asserted("id-1", "id-2"), distinct("id-1", "id-2")},
			kind: graph.DistinctFrom, a: "id-1", b: "id-2", want: graph.EffectSplit,
		},
		{
			name: "distinct_from opens when already apart", setup: []graph.Proposal{distinct("id-1", "id-2")},
			kind: graph.DistinctFrom, a: "id-1", b: "id-2", want: graph.EffectNone,
		},
		{
			name: "distinct_from closes apart to together", setup: []graph.Proposal{distinct("id-1", "id-2"), asserted("id-1", "id-2")},
			act:  closeRow(graph.DistinctFrom, "id-1", "id-2"),
			kind: graph.DistinctFrom, a: "id-1", b: "id-2", closing: true, want: graph.EffectJoined,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := ingestStore(t)
			people(t, s, "p:a/1", "p:b/1", "p:c/1")
			for _, p := range tc.setup {
				link(t, s, p)
			}
			if tc.act != nil {
				tc.act(t, s)
			}
			l := rowOf(t, s, tc.kind, tc.a, tc.b)
			got := l.OpenEffect
			if tc.closing {
				got = l.CloseEffect
			}
			if got != tc.want {
				t.Errorf("got %q, want %q (row %+v)", got, tc.want, l)
			}
		})
	}
}

func TestLinkRecordsRefusedForASameAsAcrossAKindLine(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	ingestOK(t, s, "src", obs("o1", 0, "Person", "p:a/1"), obs("o2", 1, "Team", "t:a/1"))
	link(t, s, sameAs("id-1", "id-2", graph.Asserted, "rule/x"))
	if got := rowOf(t, s, graph.SameAs, "id-1", "id-2").OpenEffect; got != graph.EffectRefused {
		t.Errorf("got open effect %q, want refused", got)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-2"})
}

func TestLinkLeavesProposalsWithoutEffects(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent"))
	l := links(t, s, graph.LinkFilter{})[0]
	if err := s.CloseLink(t.Context(), l.ID, at(70)); err != nil {
		t.Fatal(err)
	}
	l = links(t, s, graph.LinkFilter{WithClosed: true})[0]
	if l.OpenEffect != "" || l.CloseEffect != "" {
		t.Errorf("got %q/%q, want no effects on a proposal", l.OpenEffect, l.CloseEffect)
	}
}

func TestDecideSplitOfRecordsJoinedThroughAThirdOpensASplittingDistinctFrom(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1", "p:c/1")
	link(t, s, sameAs("id-1", "id-3", graph.Asserted, "rule/x"), sameAs("id-2", "id-3", graph.Asserted, "rule/x"))
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-1", "id-3": "id-1"})
	decide(t, s, graph.Decision{ID: "ev-1", Kind: graph.Split, A: "id-1", B: "id-2"})
	d := rowOf(t, s, graph.DistinctFrom, "id-1", "id-2")
	if d.OpenEffect != graph.EffectSplit || d.OpenDecision != "ev-1" || d.CloseDecision != "" {
		t.Errorf("got %+v, want a split opened by ev-1", d)
	}
}

func TestDecideJoinOverADistinctFromGivesBothRowsTheDecisionID(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	link(t, s, distinct("id-1", "id-2"))
	decide(t, s, graph.Decision{ID: "ev-1", Kind: graph.Join, A: "id-1", B: "id-2"})
	d, sa := rowOf(t, s, graph.DistinctFrom, "id-1", "id-2"), rowOf(t, s, graph.SameAs, "id-1", "id-2")
	if d.CloseDecision != "ev-1" || d.CloseEffect != graph.EffectJoined || sa.OpenDecision != "ev-1" || sa.OpenEffect != graph.EffectJoined {
		t.Errorf("got distinct_from %+v and same_as %+v, want both joined by ev-1", d, sa)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-1"})
}

func TestLinkPromotionClosesTheProposalAndOpensAnAssertedLink(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	proposal := sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent")
	proposal.Deposit.At = at(10)
	link(t, s, proposal)
	old := links(t, s, graph.LinkFilter{})[0]

	decide(t, s, graph.Decision{ID: "ev-1", Kind: graph.Accept, LinkID: old.ID, At: at(20), Reason: "same person"})

	closed := links(t, s, graph.LinkFilter{ID: old.ID, WithClosed: true})[0]
	if !closed.ValidTo.Equal(at(20)) || closed.Status != graph.Proposed || closed.Rule != "matcher/transparent" ||
		closed.CloseDecision != "ev-1" || closed.CloseEffect != "" {
		t.Errorf("got %+v, want the proposal closed at t2 by ev-1 with its rule kept", closed)
	}
	// A window holding t2 but not t1 returns the asserted row.
	win := links(t, s, graph.LinkFilter{ChangedSince: at(15), ChangedUntil: at(25), Status: graph.Asserted})
	if len(win) != 1 {
		t.Fatalf("got %+v, want the asserted link", win)
	}
	l := win[0]
	if l.PromotedFrom != old.ID || !l.ValidFrom.Equal(at(20)) || l.Rule != graph.PersonRule || l.OpenDecision != "ev-1" ||
		l.OpenEffect != graph.EffectJoined || len(l.Deposits) != 1 || l.Deposits[0].Actor != "local:501" || l.Deposits[0].Reason != "same person" {
		t.Errorf("got %+v, want asserted by person, promoted from %s at t2", l, old.ID)
	}
	if got := links(t, s, graph.LinkFilter{ChangedSince: at(5), ChangedUntil: at(15)}); len(got) != 1 || got[0].ID != old.ID {
		t.Errorf("got %+v, want only the proposal in a window around t1", got)
	}
	if got := links(t, s, graph.LinkFilter{ChangedSince: at(30)}); len(got) != 0 {
		t.Errorf("got %+v, want nothing after t2", got)
	}

	// A decision on the old proposal's ID reaches the asserted link that replaced it.
	if r := decide(t, s, graph.Decision{ID: "ev-2", Kind: graph.Accept, LinkID: old.ID}); r != (graph.LinkResult{}) {
		t.Errorf("got %+v, want a no-op accept", r)
	}
	_, err := s.Decide(t.Context(), graph.Decision{ID: "ev-3", Actor: "local:501", Kind: graph.Reject, LinkID: old.ID})
	if !errors.Is(err, graph.ErrInvalidDecision) {
		t.Errorf("got %v, want a reject of the promoted (now asserted) link refused as invalid", err)
	}
	if open := links(t, s, graph.LinkFilter{Status: graph.Asserted}); len(open) != 1 || open[0].PromotedFrom != old.ID {
		t.Errorf("got %+v, want the asserted link still open", open)
	}
}

func TestLinkPromotionByASettingsRuleKeepsTheRuleName(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent"))
	link(t, s, sameAs("id-1", "id-2", graph.Asserted, "rule/teams"))
	got := links(t, s, graph.LinkFilter{Status: graph.Asserted})
	if len(got) != 1 || got[0].Rule != "rule/teams" || got[0].PromotedFrom == "" || got[0].OpenDecision != "" {
		t.Errorf("got %+v, want an asserted link by rule/teams promoted from the proposal, with no decision", got)
	}
}
