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
