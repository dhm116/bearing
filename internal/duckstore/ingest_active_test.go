package duckstore

import (
	"strconv"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// A membership can be sent from both ends: the Person sends member_of out
// and the Team lists it in. Closing ignores a fact only inactive kinds sent,
// but an active end that stops sending it still closes it (#305).
func TestIngestActiveKindsDecideWhichFactsCompleteAndSnapshotClose(t *testing.T) {
	t.Parallel()
	memberIn := func(d *modelv1alpha1.ObservationData) {
		d.Relations = append(d.Relations, &modelv1alpha1.Relation{Type: "member_of", End: &modelv1alpha1.Relation_From{From: "k:user/ana"}})
	}
	for _, tt := range []struct {
		name                   string
		personSends, teamSends bool
		active                 []model.Kind
		wantClosed             bool
	}{
		{"both send, Person active", true, true, []model.Kind{"Person"}, true},
		{"both send, Team active", true, true, []model.Kind{"Team"}, true},
		{"only the Person sends, Person active", true, false, []model.Kind{"Person"}, true},
		{"only the Team sends, Person active", false, true, []model.Kind{"Person"}, false},
		{"both send, neither active", true, true, []model.Kind{"Component"}, false},
		{"only the Team sends, no filter", false, true, nil, true},
	} {
		for _, mode := range []string{"complete", "snapshot"} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				s := ingestStore(t)
				var person, team []func(*modelv1alpha1.ObservationData)
				if tt.personSends {
					person = append(person, rel("member_of", "k:team/pay", false))
				}
				if tt.teamSends {
					team = append(team, memberIn)
				}
				ingestOK(t, s, "gh", obs("o1", 0, "Person", "k:user/ana", person...))
				ingestOK(t, s, "gh", obs("o2", 1, "Team", "k:team/pay", team...))
				req := graph.IngestRequest{Source: "gh", Active: tt.active}
				if mode == "complete" {
					// Ana is not listed again, the Team is.
					req.Observations = append(req.Observations, obs("o3", 10, "Team", "k:team/pay"))
					req.Complete, req.SyncStarted = []model.Kind{"Person", "Component"}, at(10)
				} else {
					req.Observations = append(req.Observations, obs("o3", 10, "Person", "k:user/ana", snapshot("member_of")))
				}
				ingestReq(t, s, req)
				end, from, n := "open", "1", 1
				if tt.personSends {
					from = "0"
				}
				if tt.personSends && tt.teamSends {
					n = 2
				}
				// Complete is narrowed to Person, which the Team-only filter does not keep.
				if tt.wantClosed && (mode != "complete" || tt.name != "both send, Team active") {
					end = "10"
				}
				wantFacts(t, s, "ana member_of pay ["+from+","+end+") x"+strconv.Itoa(n))
			})
		}
	}
}

func TestIngestNarrowsCompleteToTheActiveKinds(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	ingestOK(t, s, "gh", obs("o1", 0, "Person", "k:user/ana", attrs("language", "x")))
	ingestOK(t, s, "gh", obs("o2", 0, "Team", "k:team/pay", attrs("tier", "1")))
	ingestReq(t, s, graph.IngestRequest{
		Source: "gh", Complete: []model.Kind{"Person", "Team"}, SyncStarted: at(10), Active: []model.Kind{"Team"},
	})
	// Only Team is active, so only Team's records can read as gone.
	wantFacts(t, s, `ana language "x" [0,open) x1`, `pay tier "1" [0,10) x1`)
}
