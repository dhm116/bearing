package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/query"
)

// TestCLIRESTAndMCPGiveTheSameAnswer asks each question of ADR 11's answers
// three ways: the CLI over the store, the REST API (whose body --json passes
// on), and the MCP tool's structuredContent. The catalog also names an owner
// of payments-api, a team the matcher proposes is the one a directory names,
// so the owners answer carries a relation.
func TestCLIRESTAndMCPGiveTheSameAnswer(t *testing.T) {
	w := newWorld(t)
	w.ingest(catalogSource, seedAt.Add(2*time.Minute),
		entity("Team", "catalog:team/payments", "payments"),
		entity("Repository", "github:repo/acme/payments-api", "payments-api", rel("owned_by", "catalog:team/payments")))
	w.ingest("directory-acme", seedAt.Add(3*time.Minute), // a time of its own: an observation's ID is its key and time
		entity("Repository", "github:repo/acme/payments-api", "payments-api", rel("owned_by", "github:team/acme/payments")))
	cat, err := w.store.Entity(context.Background(), "catalog:team/payments")
	if err != nil {
		t.Fatal(err)
	}
	gh, err := w.store.Entity(context.Background(), "github:team/acme/payments")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Link(context.Background(), []graph.Proposal{{
		A: cat.ID, B: gh.ID, Kind: graph.SameAs, Status: graph.Proposed, Rule: "match/team",
		Deposit: graph.Deposit{At: seedAt.Add(4 * time.Minute), Weight: 0.8, Features: map[string]float64{"shared_members": 0.9}},
	}}); err != nil {
		t.Fatal(err)
	}
	addr := w.serve()
	for _, tt := range []struct {
		name      string
		cli       []string
		tool, arg string
	}{
		{"owners", []string{"owner", "acme/payments-api", "--members"}, "owners", `{"ref":"github:repo/acme/payments-api"}`},
		{"owners in the past", []string{"owner", "acme/payments-api", "--members", "--as-of", "2026-10-01T01:00:00Z"}, "owners", `{"ref":"github:repo/acme/payments-api","as_of":"2026-10-01T01:00:00Z"}`},
		{"depended on by", []string{"related", "catalog:component/db", "--predicate", "depends_on", "--direction", "in", "--depth", "2"}, "depended_on_by", `{"ref":"catalog:component/db","depth":2}`},
		{"changes", []string{"changes", "--since", "2026-10-01T01:00:00Z"}, "changes", `{"since":"2026-10-01T01:00:00Z"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fromCLI, err := w.cli(append(tt.cli, "--json")...)
			if err != nil {
				t.Fatal(err)
			}
			fromREST, err := w.cliServer(addr, append(tt.cli, "--json")...)
			if err != nil {
				t.Fatal(err)
			}
			res := result(t, w.mcpRun("", "", initAt("2025-06-18"), callReq(1, tt.tool, tt.arg))[1])
			var cli, rest any
			if err := json.Unmarshal([]byte(fromCLI), &cli); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(fromREST), &rest); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cli, rest) {
				t.Errorf("REST differs from the CLI:\n--- rest\n%s\n--- cli\n%s", fromREST, fromCLI)
			}
			if mcp := res["structuredContent"]; !reflect.DeepEqual(cli, mcp) {
				t.Errorf("MCP differs from the CLI:\n--- mcp\n%v\n--- cli\n%s", mcp, fromCLI)
			}
		})
	}
	var o query.Ownership
	ask(t, w, &o, "owner", "acme/payments-api")
	if len(o.Relations) != 1 || o.Relations[0].Kind != query.RelationLikelySame {
		t.Errorf("got relations %+v, want the two payments teams likely the same", o.Relations)
	}
}

// TestCLISaysLikelyForAProposal renders a proposed claim and a proposed
// relation: each is worded "likely", never as a fact.
func TestCLISaysLikelyForAProposal(t *testing.T) {
	team := func(id, name string) *query.Ref { return &query.Ref{ID: id, Cluster: id, Kind: "Team", Name: name} }
	fact := func(status string) query.Fact {
		return query.Fact{
			Subject: query.Ref{ID: "r", Kind: "Repository", Name: "frontend-spa"}, Predicate: "owned_by", Object: team("t1", "design"),
			Status: status, Sources: []query.Source{{Source: "github-acme"}},
		}
	}
	for _, tt := range []struct {
		status, want string
	}{
		{"asserted", "github-acme says frontend-spa (Repository) owned_by design (Team)."},
		{"proposed", "github-acme says frontend-spa (Repository) likely owned_by design (Team)."},
	} {
		if got := claimSentence(query.Claim{Fact: fact(tt.status)}, time.Time{}); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
	ui := query.Claim{Fact: fact("asserted")}
	ui.Fact.Object = team("t2", "UI Team")
	claims := []query.Claim{{Fact: fact("asserted")}, ui}
	rel := query.Relation{Clusters: [2]string{"t1", "t2"}, Kind: query.RelationLikelySame, Links: []query.Link{{
		Status: "proposed", Kind: "same_as",
		Score: query.LinkScore{Strength: 0.92}, Deposits: []query.LinkDeposit{{Weight: 1, Features: map[string]float64{"shared_members": 0.92}}},
	}}}
	want := "design (Team) and UI Team (Team) are likely the same (strength 92%; shared_members 0.92)."
	if got := relationSentence(rel, claims); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	var b strings.Builder
	if err := renderOwnership(&b, &query.Ownership{Answer: query.Answer{
		Subjects: []query.Subject{{Ref: query.Ref{ID: "r", Name: "frontend"}, Match: query.MatchFuzzy, Score: 0.93}},
		Claims:   claims, Relations: []query.Relation{rel}, LimitHit: true,
	}}, time.Time{}, "frontnd"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`owners of "frontnd": 1 record(s) match`, "frontend [r]: fuzzily, 93%", "more match than shown", "are likely the same"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, b.String())
		}
	}
}

// TestRelationSentencesWordEachRule renders one relation of each kind.
func TestRelationSentencesWordEachRule(t *testing.T) {
	team := func(cluster, name string) query.Claim {
		return query.Claim{Fact: query.Fact{Object: &query.Ref{ID: cluster, Cluster: cluster, Name: name, ClusterKind: "Team"}}}
	}
	claims := []query.Claim{team("c1", "design"), team("c2", "UI Team")}
	proposal := query.Link{Status: "proposed", Kind: "distinct_from", Score: query.LinkScore{Strength: 0.5}}
	for _, tt := range []struct {
		rel  query.Relation
		want string
	}{
		{
			query.Relation{Kind: query.RelationDistinct, Links: []query.Link{{Status: "asserted"}}},
			"design (Team) and UI Team (Team) are not the same: an asserted distinct_from says so.",
		},
		{
			query.Relation{Kind: query.RelationDistinct, Links: []query.Link{{Status: "asserted"}, proposal}},
			"design (Team) and UI Team (Team) are not the same: an asserted distinct_from says so. A proposal says otherwise (strength 50%).",
		},
		{
			query.Relation{Kind: query.RelationAssertedNotJoined, Reason: query.ReasonKinds},
			"An asserted same_as names design (Team) and UI Team (Team) as one, but they are not joined: their kinds differ.",
		},
		{
			query.Relation{Kind: query.RelationAssertedNotJoined, Reason: query.ReasonDistinct},
			"An asserted same_as names design (Team) and UI Team (Team) as one, but they are not joined: a distinct_from keeps them apart.",
		},
		{
			query.Relation{Kind: query.RelationLikelyDistinct, Links: []query.Link{proposal}},
			"design (Team) and UI Team (Team) are likely not the same (strength 50%).",
		},
	} {
		tt.rel.Clusters = [2]string{"c1", "c2"}
		if got := relationSentence(tt.rel, claims); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.rel.Kind, got, tt.want)
		}
	}
	if got := clusterName("c9", claims); got != "c9" {
		t.Errorf("a cluster no claim names: got %q, want its ID", got)
	}
}

// TestChangesTextListsJoinsWithTheirJustification renders a person's join
// over a distinct_from and a promotion's row, from the store's history.
func TestChangesTextListsJoinsWithTheirJustification(t *testing.T) {
	w := newWorld(t)
	w.ingest(catalogSource, seedAt.Add(2*time.Minute), entity("Team", "catalog:team/payments", "payments"))
	cat, err := w.store.Entity(context.Background(), "catalog:team/payments")
	if err != nil {
		t.Fatal(err)
	}
	gh, err := w.store.Entity(context.Background(), "github:team/acme/payments")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Link(context.Background(), []graph.Proposal{{
		A: cat.ID, B: gh.ID, Kind: graph.SameAs, Status: graph.Proposed, Rule: "match/team",
		Deposit: graph.Deposit{At: seedAt.Add(3 * time.Minute), Weight: 0.8},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.Decide(context.Background(), graph.Decision{
		ID: "d1", Kind: graph.Join, A: string(cat.ID), B: string(gh.ID),
		Actor: "alice", Reason: "one team", At: seedAt.Add(4 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := w.cli("changes", "--since", "2026-10-01T00:03:30Z", "--as-of", "2026-10-01T00:05:00Z")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2026-10-01T00:04:00Z joined", "payments [Team " + string(cat.ID) + "]",
		`by person, decided by alice: "one team" (decision d1)`, "same_as opened, joined, promoted from ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("changes lack %q:\n%s", want, got)
		}
	}
}

// TestRenderedAnswersSayWhatIsMissing renders the rare parts of answers: no
// owner, a team with no one on it or more than shown, a fuzzy start with a
// same_as hop, a fact with no sources, and a join an observation justified.
func TestRenderedAnswersSayWhatIsMissing(t *testing.T) {
	repo := query.Ref{ID: "r1", Cluster: "r1", Kind: "Repository", Name: "payments"}
	alias := query.Ref{ID: "r2", Cluster: "r1", Kind: "Repository", Name: "payments-svc"}
	team := query.Ref{ID: "t1", Cluster: "t1", Kind: "Team", Name: "Payments"}
	render := func(f func(*strings.Builder) error) string {
		var b strings.Builder
		if err := f(&b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	fuzzy := query.Subject{Ref: repo, Match: query.MatchFuzzy, Score: 0.9}
	claim := query.Claim{
		Fact:  query.Fact{Subject: alias, Predicate: "owned_by", Object: &team, Status: "asserted"},
		Start: fuzzy, Facts: 1, SameAs: 1,
		Path: []query.PathStep{{From: repo, To: &alias, Links: []string{"l1"}}, {From: alias, To: &team, FactID: "f1", Predicate: "owned_by"}},
	}
	for name, tt := range map[string]struct {
		got  string
		want []string
	}{
		"no owner": {render(func(b *strings.Builder) error {
			return renderOwnership(b, &query.Ownership{
				Answer: query.Answer{Subjects: []query.Subject{{Ref: repo, Match: query.MatchKey, Score: 1}}},
				Teams:  []query.TeamMembers{{Team: team, Members: []query.Member{}, Truncated: true}},
			}, time.Time{}, "payments")
		}), []string{"no asserted owner", "people on Payments", "  none", "more people than shown"}},
		"a fuzzy start through a hop": {render(func(b *strings.Builder) error {
			return renderOwnership(b, &query.Ownership{Answer: query.Answer{Subjects: []query.Subject{fuzzy}, Claims: []query.Claim{claim}}}, time.Time{}, "paymnts")
		}), []string{
			"Bearing says payments-svc (Repository) owned_by Payments (Team).",
			"starts at payments [Repository r1], which only resembles the question (90%)",
			"path: payments (Repository) =same_as(l1)= payments-svc (Repository) -owned_by-> Payments (Team)",
		}},
		"nothing related": {render(func(b *strings.Builder) error {
			return renderRelated(b, &query.Related{Answer: query.Answer{Subjects: []query.Subject{{Ref: repo, Match: query.MatchKey, Score: 1}}}, Direction: "in"}, time.Time{}, "payments")
		}), []string{"direction in, as of now", "\nnone\n"}},
		"a join by an observation": {render(func(b *strings.Builder) error {
			return renderChanges(b, &query.Changes{Identity: []query.IdentityChange{{
				Kind: query.IdentityJoined, A: query.LinkRecord{ID: "r1"}, B: query.LinkRecord{ID: "r2"},
				Justification: query.Justification{Rule: "linked_ids", ObservationID: "o1"},
			}}}, "")
		}), []string{"joined", "by linked_ids, observation o1"}},
	} {
		for _, w := range tt.want {
			if !strings.Contains(tt.got, w) {
				t.Errorf("%s: output lacks %q:\n%s", name, w, tt.got)
			}
		}
	}
	if got := pathText(nil); got != "" {
		t.Errorf("an empty path: got %q, want nothing", got)
	}
}
