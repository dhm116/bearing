package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

const (
	teamPayments = "github:team/acme/payments"
	teamPlatform = "github:team/acme/platform"
)

// propose opens a proposed same_as between the world's two teams and returns
// the link's ID.
func (w *world) propose() string {
	w.t.Helper()
	ctx := context.Background()
	a, err := w.store.Entity(ctx, teamPayments)
	if err != nil {
		w.t.Fatal(err)
	}
	b, err := w.store.Entity(ctx, teamPlatform)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.store.Link(ctx, []graph.Proposal{{
		A: a.ID, B: b.ID, Kind: graph.SameAs, Status: graph.Proposed, Rule: "matcher/transparent",
		Deposit: graph.Deposit{At: seedAt, Weight: 0.8, Features: map[string]float64{"name": 0.8}},
	}}); err != nil {
		w.t.Fatal(err)
	}
	ls, err := w.store.Links(ctx, graph.LinkFilter{Status: graph.Proposed})
	if err != nil || len(ls) != 1 {
		w.t.Fatalf("got %v, %v, want one proposed link", ls, err)
	}
	return ls[0].ID
}

// decisions are the LinkDecided events on the world's log.
func (w *world) decisions() []*eventv1alpha1.LinkDecided {
	w.t.Helper()
	es, err := w.store.Read(context.Background(), "core/identity", 0, store.MaxReadEntries)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []*eventv1alpha1.LinkDecided
	for _, e := range es {
		m := &eventv1alpha1.LinkDecided{}
		if err := model.DecodeJSON(e.Data, m); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestLinksListsTheReviewQueueFromAStoreAndFromAServer(t *testing.T) {
	w := newWorld(t)
	id := w.propose()
	fromStore, err := w.cli("links", "--proposed")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 proposed identity links", id, "same_as proposed by matcher/transparent", "strength", "Payments", "Platform", "github-acme", "name=0.80"} {
		if !strings.Contains(fromStore, want) {
			t.Errorf("got %q, want it to contain %q", fromStore, want)
		}
	}
	addr := w.serve()
	for _, args := range [][]string{{"links", "--proposed"}, {"links", "--entity", teamPayments, "--limit", "5"}, {"links", "--proposed", "--json"}} {
		fromServer, err := w.cliServer(addr, args...)
		if err != nil {
			t.Fatal(err)
		}
		again, err := w.cli(args...)
		if err != nil {
			t.Fatal(err)
		}
		if fromServer != again {
			t.Errorf("%v: the server answered\n%s\nthe store answered\n%s", args, fromServer, again)
		}
	}
	if none, err := w.cli("links", "--entity", "github:user/jdoe"); err != nil || !strings.Contains(none, "no open identity links") {
		t.Errorf("got %q, %v, want no links for a person with none", none, err)
	}
}

func TestLinkDecisionsGoThroughTheServersLog(t *testing.T) {
	w := newWorld(t)
	id := w.propose()
	addr := w.serve()
	run := func(args ...string) string {
		t.Helper()
		out, err := w.cliWith(map[string]string{tokenEnv: apiToken}, append(append([]string{"link"}, args...), "--server", addr)...)
		if err != nil {
			t.Fatalf("link %v: %v", args, err)
		}
		return out
	}
	if out := run("accept", id, "--reason", "same team"); !strings.Contains(out, "logged") {
		t.Errorf("got %q, want the decision logged", out)
	}
	run("reject", id)
	run("join", teamPayments, teamPlatform)
	run("split", teamPayments, teamPlatform)
	ctx := context.Background()
	a, _ := w.store.Entity(ctx, teamPayments)
	b, _ := w.store.Entity(ctx, teamPlatform)
	if _, err := w.store.Link(ctx, []graph.Proposal{{A: a.ID, B: b.ID, Kind: graph.DistinctFrom, Status: graph.Asserted, Rule: "person", Deposit: graph.Deposit{At: seedAt, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	apart, err := w.store.Links(ctx, graph.LinkFilter{Kind: graph.DistinctFrom})
	if err != nil || len(apart) != 1 {
		t.Fatalf("got %v, %v, want one distinct_from", apart, err)
	}
	run("undo-distinct", apart[0].ID)
	var got []string
	for _, d := range w.decisions() {
		got = append(got, strings.TrimPrefix(d.GetDecision().String(), "DECISION_"))
		if d.GetActor().GetSubject() != "token" || d.GetActor().GetAgent() {
			t.Errorf("got actor %v, want the token's caller, not an agent", d.GetActor())
		}
	}
	if want := "ACCEPT REJECT JOIN SPLIT UNDO_DISTINCT"; strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	if first := w.decisions()[0]; first.GetLinkId() != id || first.GetReason() != "same team" {
		t.Errorf("got %v, want the accept of %s with its reason", first, id)
	}
	if j := w.decisions()[2]; j.GetA() != string(a.ID) || j.GetB() != string(b.ID) {
		t.Errorf("got %v, want the join of the two teams' records, %s and %s", j, a.ID, b.ID)
	}
	if out := run("accept", id, "--json"); !strings.Contains(out, `"event_id"`) {
		t.Errorf("got %q, want the server's answer as JSON", out)
	}
}

func TestLinkDecisionsAreRefusedBeforeTheyAreSent(t *testing.T) {
	w := newWorld(t)
	addr := w.serve()
	for name, tt := range map[string]struct {
		vars map[string]string
		args []string
		want string
	}{
		"no subcommand":      {nil, []string{"link"}, ""},
		"an unknown one":     {nil, []string{"link", "promote", "x"}, ""},
		"accept with two":    {nil, []string{"link", "accept", "a", "b", "--server", addr}, "a link ID"},
		"join with one":      {nil, []string{"link", "join", "a", "--server", addr}, "two records"},
		"a store":            {nil, []string{"link", "accept", "x", "--store", "mem://"}, "running server"},
		"no server":          {nil, []string{"link", "accept", "x"}, "running server"},
		"a store in the env": {map[string]string{storeEnv: "mem://"}, []string{"link", "accept", "x"}, "running server"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.cliWith(tt.vars, tt.args...)
			if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want a usage error containing %q", err, tt.want)
			}
		})
	}
	if n := len(w.decisions()); n != 0 {
		t.Errorf("%d decisions were sent, want none", n)
	}
	// The server's refusals reach the caller as the same kinds of error.
	_, err := w.cliWith(map[string]string{tokenEnv: apiToken}, "link", "accept", "nope", "--server", addr)
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "not_found") {
		t.Errorf("got %v, want a not-found naming the link", err)
	}
}
