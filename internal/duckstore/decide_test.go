package duckstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
)

func decide(t *testing.T, s *Store, d graph.Decision) graph.LinkResult {
	t.Helper()
	if d.Actor == "" {
		d.Actor = "local:501"
	}
	r, err := s.Decide(t.Context(), d)
	if err != nil {
		t.Fatalf("decide %+v: %v", d, err)
	}
	return r
}

func TestDecideAcceptAssertsAProposalOnceAndLeavesTheDeciderAsEvidence(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1") // id-1, id-2
	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent"))
	l := links(t, s, graph.LinkFilter{})[0]
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-2"})

	d := graph.Decision{ID: "ev-1", Kind: graph.Accept, LinkID: l.ID, Reason: "same person"}
	if r := decide(t, s, d); r.Reinforced != 1 || r.Opened != 0 {
		t.Fatalf("got %+v, want the proposal promoted", r)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-1"})
	got := links(t, s, graph.LinkFilter{ID: l.ID})
	last := got[0].Deposits[len(got[0].Deposits)-1]
	if got[0].Status != graph.Asserted || got[0].Rule != "person" || last.Actor != "local:501" || last.Reason != "same person" || last.Weight != 1 {
		t.Fatalf("got %+v, want asserted by person with the actor and reason on the last deposit", got[0])
	}

	// The same event again, and a new event on an asserted link, change nothing.
	for _, id := range []string{"ev-1", "ev-2"} {
		d.ID = id
		if r := decide(t, s, d); r != (graph.LinkResult{}) {
			t.Errorf("event %s: got %+v, want a no-op", id, r)
		}
	}
	if again := links(t, s, graph.LinkFilter{ID: l.ID}); len(again[0].Deposits) != len(got[0].Deposits) {
		t.Errorf("got %d deposits, want %d", len(again[0].Deposits), len(got[0].Deposits))
	}
}

func TestDecideRejectClosesTheProposalAndKeepsThePairApart(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent"))
	l := links(t, s, graph.LinkFilter{})[0]

	if r := decide(t, s, graph.Decision{ID: "ev-1", Kind: graph.Reject, LinkID: l.ID}); r.Closed != 1 || r.Opened != 1 {
		t.Fatalf("got %+v, want one closed and one opened", r)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-2"})
	if open := links(t, s, graph.LinkFilter{Kind: graph.SameAs}); len(open) != 0 {
		t.Errorf("got %d open same_as links, want none", len(open))
	}
	apart := links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom, Status: graph.Asserted})
	if len(apart) != 1 || apart[0].Rule != "person" || apart[0].Deposits[0].Actor != "local:501" {
		t.Fatalf("got %+v, want one asserted distinct_from by the person", apart)
	}
	// A later accept of the closed proposal is a no-op.
	if r := decide(t, s, graph.Decision{ID: "ev-2", Kind: graph.Accept, LinkID: l.ID}); r != (graph.LinkResult{}) {
		t.Errorf("got %+v, want accepting a closed link to do nothing", r)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-2": "id-2"})
}

func TestDecideJoinSplitJoinAgainAndUndoDistinct(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1", "p:c/1") // id-1..3
	join := func(id, a, b string) { decide(t, s, graph.Decision{ID: id, Kind: graph.Join, A: a, B: b}) }

	join("e1", "p:a/1", "id-2") // a key and a record ID
	join("e2", "id-3", "id-2")
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-1", "id-3": "id-1"})

	decide(t, s, graph.Decision{ID: "e3", Kind: graph.Split, A: "id-1", B: "id-2"})
	// id-1 and id-2 are apart; id-3 joined to id-2 stays with it.
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-2", "id-3": "id-2"})
	apart := links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom})
	if len(apart) != 1 {
		t.Fatalf("got %+v, want one distinct_from", apart)
	}

	join("e4", "id-1", "id-2") // joining again overrides the distinct_from
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-1", "id-3": "id-1"})
	if open := links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom}); len(open) != 0 {
		t.Errorf("got %+v, want the distinct_from closed by the join", open)
	}

	decide(t, s, graph.Decision{ID: "e5", Kind: graph.Split, A: "id-1", B: "id-2"})
	decide(t, s, graph.Decision{ID: "e6", Kind: graph.UndoDistinct, LinkID: links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom})[0].ID})
	if open := links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom}); len(open) != 0 {
		t.Errorf("got %+v, want no open distinct_from after the undo", open)
	}
	// The split closed the same_as, so the pair stays apart until a join.
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-2"})
}

func TestDecideRefusesWhatItCannotDoAndWritesNothing(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	ingestOK(t, s, "src", obs1("t", 1, "Team", "p:team/1"))
	link(t, s, sameAs("id-1", "id-2", graph.Asserted, "rule/x"))
	asserted := links(t, s, graph.LinkFilter{})[0]
	for _, tt := range []struct {
		name string
		d    graph.Decision
		want error
	}{
		{"unknown link", graph.Decision{ID: "e", Kind: graph.Accept, LinkID: "nope"}, graph.ErrNotFound},
		{"unknown record", graph.Decision{ID: "e", Kind: graph.Join, A: "id-1", B: "nope"}, graph.ErrNotFound},
		{"one record", graph.Decision{ID: "e", Kind: graph.Join, A: "id-1", B: "p:a/1"}, graph.ErrInvalidDecision},
		{"different kinds", graph.Decision{ID: "e", Kind: graph.Join, A: "id-1", B: "p:team/1"}, graph.ErrInvalidDecision},
		{"reject an asserted link", graph.Decision{ID: "e", Kind: graph.Reject, LinkID: asserted.ID}, graph.ErrInvalidDecision},
		{"undo a same_as", graph.Decision{ID: "e", Kind: graph.UndoDistinct, LinkID: asserted.ID}, graph.ErrInvalidDecision},
		{"no kind", graph.Decision{ID: "e", LinkID: asserted.ID}, graph.ErrInvalidDecision},
		{"no actor", graph.Decision{ID: "e", Kind: graph.Accept, LinkID: asserted.ID, Actor: ""}, graph.ErrInvalidDecision},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name != "no actor" {
				tt.d.Actor = "local:501"
			}
			if _, err := s.Decide(t.Context(), tt.d); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	if got := links(t, s, graph.LinkFilter{WithClosed: true}); len(got) != 1 || len(got[0].Deposits) != 1 || !got[0].ValidTo.IsZero() {
		t.Errorf("got %+v, want the one link untouched", got)
	}
	// A refused decision leaves no note, so the same event can succeed later.
	decide(t, s, graph.Decision{ID: "e", Kind: graph.Split, A: "id-1", B: "id-2"})
}

func TestDecideRejectBlocksTheMatcherUntilTheDistinctFromIsUndone(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	cfg := match.Config{Now: func() time.Time { return at(2000) }}
	seen := func(n int) { // both teams observed again at minute n
		ingestOK(t, s, "gh", obs(fmt.Sprintf("g%d", n), n, "Team", "gh:team/payments", attrs("name", "payments")))
		ingestOK(t, s, "dir", obs(fmt.Sprintf("d%d", n), n, "Team", "dir:group/payments", attrs("name", "payments")))
	}
	run := func() match.Result {
		r, err := match.Run(t.Context(), s, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	seen(10)
	if r := run(); r.Proposed != 1 {
		t.Fatalf("got %+v, want the matcher to propose the pair", r)
	}
	decide(t, s, graph.Decision{ID: "e1", Kind: graph.Reject, LinkID: links(t, s, graph.LinkFilter{Kind: graph.SameAs})[0].ID})
	seen(20)
	if r := run(); r.Proposed != 0 || len(r.Review) != 0 {
		t.Fatalf("got %+v, want nothing proposed for a rejected pair", r)
	}
	decide(t, s, graph.Decision{ID: "e2", Kind: graph.UndoDistinct, LinkID: links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom})[0].ID})
	seen(30)
	if r := run(); r.Proposed != 1 {
		t.Fatalf("got %+v, want the pair proposed again once the distinct_from is undone", r)
	}
	if r := run(); r.Proposed != 0 || r.Reinforced != 0 {
		t.Fatalf("got %+v, want a rerun on the same evidence to add nothing", r)
	}
}

func TestDecideRejectOfAClosedLinkLeavesANewerProposalOpen(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent"))
	old := links(t, s, graph.LinkFilter{})[0]
	if err := s.CloseLink(t.Context(), old.ID, at(60)); err != nil {
		t.Fatal(err)
	}
	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent"))
	if r := decide(t, s, graph.Decision{ID: "e1", Kind: graph.Reject, LinkID: old.ID}); r != (graph.LinkResult{}) {
		t.Errorf("got %+v, want rejecting a closed link to do nothing", r)
	}
	if open := links(t, s, graph.LinkFilter{}); len(open) != 1 || open[0].ID == old.ID || open[0].Kind != graph.SameAs {
		t.Errorf("got %+v, want the newer proposal open and no distinct_from", open)
	}
}

func TestDecideJoinAndSplitOfAPairAlreadyThatWayChangeNothing(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	for _, k := range []graph.DecisionKind{graph.Join, graph.Split} {
		decide(t, s, graph.Decision{ID: string(k) + "-1", Kind: k, A: "id-1", B: "id-2"})
		before := links(t, s, graph.LinkFilter{WithClosed: true})
		if r := decide(t, s, graph.Decision{ID: string(k) + "-2", Kind: k, A: "id-1", B: "id-2"}); r != (graph.LinkResult{}) {
			t.Errorf("%s again: got %+v, want nothing", k, r)
		}
		if after := links(t, s, graph.LinkFilter{WithClosed: true}); fmt.Sprint(after) != fmt.Sprint(before) {
			t.Errorf("%s again: got links %+v, want %+v", k, after, before)
		}
	}
}

// A join or accept that a distinct_from held by another record of either
// cluster would leave apart is refused, naming that link, and writes nothing.
func TestDecideJoinBlockedByAnotherRecordsDistinctFromIsRefused(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1", "p:c/1") // id-1..3
	decide(t, s, graph.Decision{ID: "e1", Kind: graph.Join, A: "id-1", B: "id-3"})
	decide(t, s, graph.Decision{ID: "e2", Kind: graph.Split, A: "id-3", B: "id-2"})
	blocker := links(t, s, graph.LinkFilter{Kind: graph.DistinctFrom})[0].ID
	later := sameAs("id-1", "id-2", graph.Proposed, "matcher/transparent")
	later.Deposit.At = at(2000) // after the join of id-1 and id-3, which clustering takes first
	link(t, s, later)
	proposal := links(t, s, graph.LinkFilter{Status: graph.Proposed})[0].ID
	before := links(t, s, graph.LinkFilter{WithClosed: true})
	for _, d := range []graph.Decision{
		{ID: "e3", Kind: graph.Join, A: "id-1", B: "id-2", Actor: "local:501"},
		{ID: "e4", Kind: graph.Accept, LinkID: proposal, Actor: "local:501"},
	} {
		_, err := s.Decide(t.Context(), d)
		if !errors.Is(err, graph.ErrInvalidDecision) || !strings.Contains(err.Error(), blocker) {
			t.Errorf("%s: got %v, want an invalid decision naming %s", d.Kind, err, blocker)
		}
	}
	if after := links(t, s, graph.LinkFilter{WithClosed: true}); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("got links %+v, want them untouched", after)
	}
	wantClusters(t, s, map[string]graph.EntityID{"id-1": "id-1", "id-2": "id-2", "id-3": "id-1"})
}

func TestLinksWithALimitReturnsTheNewestEvidenceFirstAndRecordsByID(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1", "p:c/1")
	old, newer := sameAs("id-1", "id-2", graph.Proposed, "m"), sameAs("id-2", "id-3", graph.Proposed, "m")
	newer.Deposit.At = at(70)
	link(t, s, old, newer)
	got := links(t, s, graph.LinkFilter{Limit: 1})
	if len(got) != 1 || got[0].A != "id-2" || len(got[0].Deposits) != 1 {
		t.Errorf("got %+v, want only the link with the newer deposit, with its deposit", got)
	}
	recs, err := s.RecordsByID(t.Context(), []graph.EntityID{"id-3", "nope", "id-1"})
	if err != nil || len(recs) != 2 || recs[0].ID != "id-1" || recs[1].ID != "id-3" || len(recs[0].Keys) != 1 {
		t.Errorf("got %+v, %v, want id-1 and id-3 with their keys", recs, err)
	}
}
