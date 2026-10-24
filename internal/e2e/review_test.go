package e2e

import (
	"context"
	"slices"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/query"
	"bearing.example/pkg/server"
)

// nopApplier applies nothing: the graph is already synced.
type nopApplier struct{}

func (nopApplier) Apply(context.Context, string, []*eventv1alpha1.Observation, server.Completion) error {
	return nil
}

// TestAcmePersonReviewsATeamLink: the matcher proposes that the GitHub
// payments team and the directory's payments group are one team; a person
// accepts, and the owner of payments-api is then the joined team; a split
// undoes it, the matcher does not propose the pair again, and a join
// restores it. Decisions travel through the server's log, as from the CLI.
func TestAcmePersonReviewsATeamLink(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.syncGitHub()
	w.syncDirectory()
	cfg := match.Config{Now: w.clock.Now}
	if _, err := match.Run(t.Context(), w.store, cfg); err != nil {
		t.Fatal(err)
	}
	q := &query.Querier{Graph: w.store, Now: w.clock.Now}
	const group = "authentik:group_name/payments"

	queue, err := q.Links(t.Context(), query.LinksRequest{Status: "proposed", Entity: payments})
	if err != nil || len(queue) != 1 || queue[0].Rule != "matcher/transparent" || queue[0].Score.Strength <= 0 || len(queue[0].Deposits[0].Features) == 0 {
		t.Fatalf("got %+v, %v, want the matcher's one proposal for the payments team, with its evidence", queue, err)
	}
	link := queue[0].ID
	ownerKeys := func() []string {
		t.Helper()
		o, err := q.Owners(t.Context(), paymentsAPI, time.Time{})
		if err != nil || len(o.Owners) != 1 {
			t.Fatalf("got %+v, %v, want one owner of payments-api", o, err)
		}
		e, err := q.Get(t.Context(), o.Owners[0].Object.ID, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return e.Keys
	}
	if keys := ownerKeys(); slices.Contains(keys, group) {
		t.Fatalf("the owner has keys %v before any decision, want the GitHub team alone", keys)
	}

	members := func() int {
		return len(slices.Compact(slices.Sorted(slices.Values(w.targets("github:user/jdoe", "member_of")))))
	}
	if n := members(); n != 2 {
		t.Fatalf("Jane Doe is a member of %d teams before any decision, want the GitHub team and the directory group", n)
	}

	srv, err := server.New(server.Config{Log: w.store, Applier: nopApplier{}, Links: w.store, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	person := &eventv1alpha1.Actor{Subject: "local:501"}
	decide := func(d server.LinkDecision) {
		t.Helper()
		logged, err := srv.DecideLink(ctx, d, person)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			off, err := w.store.Committed(ctx, server.DefaultGroup, "core/identity")
			if err != nil {
				t.Fatal(err)
			}
			if off >= logged.Offset {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the decision was not applied in time")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	decide(server.LinkDecision{Kind: graph.Accept, LinkID: link, Reason: "same team"})
	if keys := ownerKeys(); !slices.Contains(keys, group) || !slices.Contains(keys, payments) {
		t.Fatalf("after the accept the owner has keys %v, want the GitHub team and the directory group", keys)
	}
	if n := members(); n != 1 {
		t.Errorf("Jane Doe is a member of %d teams after the accept, want the one joined team", n)
	}

	decide(server.LinkDecision{Kind: graph.Split, A: payments, B: group, Reason: "different teams after all"})
	if keys := ownerKeys(); slices.Contains(keys, group) {
		t.Fatalf("after the split the owner has keys %v, want the GitHub team alone", keys)
	}
	res, err := match.Run(t.Context(), w.store, cfg)
	if err != nil || res.Proposed != 0 {
		t.Fatalf("got %+v, %v, want the matcher to leave the split pair alone", res, err)
	}
	if again, err := q.Links(t.Context(), query.LinksRequest{Status: "proposed", Entity: payments}); err != nil || len(again) != 0 {
		t.Fatalf("got %+v, %v, want nothing proposed for the split pair", again, err)
	}

	decide(server.LinkDecision{Kind: graph.Join, A: payments, B: group})
	if keys := ownerKeys(); !slices.Contains(keys, group) {
		t.Fatalf("after the join the owner has keys %v, want the directory group among them", keys)
	}
	asserted, err := q.Links(t.Context(), query.LinksRequest{Status: "asserted", Kind: "same_as", Entity: payments})
	if err != nil || len(asserted) == 0 {
		t.Fatalf("got %+v, %v, want the joining link", asserted, err)
	}
	var by []string
	for _, l := range asserted {
		for _, d := range l.Deposits {
			if d.Actor != "" {
				by = append(by, l.Rule+" by "+d.Actor+": "+d.Reason)
			}
		}
	}
	if !slices.Contains(by, "person by local:501: ") {
		t.Errorf("got %v, want the join recorded as a person's decision by local:501", by)
	}
}
