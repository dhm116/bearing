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
		closedByComplete       bool // Complete lists Person, so a Team-only filter narrows it away
		closedBySnapshot       bool
	}{
		{"both send, Person active", true, true, []model.Kind{"Person"}, true, true},
		{"both send, Team active", true, true, []model.Kind{"Team"}, false, true},
		{"only the Person sends, Person active", true, false, []model.Kind{"Person"}, true, true},
		{"only the Team sends, Person active", false, true, []model.Kind{"Person"}, false, false},
		{"both send, neither active", true, true, []model.Kind{"Component"}, false, false},
		{"only the Team sends, no filter", false, true, nil, true, true},
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
				if (mode == "complete" && tt.closedByComplete) || (mode == "snapshot" && tt.closedBySnapshot) {
					end = "10"
				}
				wantFacts(t, s, "ana member_of pay ["+from+","+end+") x"+strconv.Itoa(n))
			})
		}
	}
}

// A listing of a parent kind covers its descendants, and only the active
// ones are closed.
func TestIngestCompleteOfAParentClosesItsActiveDescendants(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		active []model.Kind
		want   string
	}{
		{"the descendant is active", []model.Kind{"k8s.Service"}, `svc language "Go" [0,10) x1`},
		{"only the parent is active", []model.Kind{model.KindComponent}, `svc language "Go" [0,open) x1`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := hierarchyStore(t)
			ingestOK(t, s, "k8s", obs("o1", 0, "k8s.Service", "k:svc/svc", attrs("language", "Go")))
			ingestReq(t, s, graph.IngestRequest{
				Source: "k8s", Complete: []model.Kind{model.KindComponent}, SyncStarted: at(10), Active: tt.active,
			})
			wantFacts(t, s, tt.want)
		})
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
