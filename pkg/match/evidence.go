package match

import (
	"strings"
	"time"

	"bearing.example/pkg/graph"
)

// Evidence is what the facts say about each cluster, for matchers to read.
type Evidence struct {
	// Members maps a cluster to the clusters that are member_of it.
	Members map[graph.EntityID]map[graph.EntityID]bool
	// Emails maps a cluster to its lower-cased email and verified_email
	// attributes.
	Emails map[graph.EntityID]map[string]bool
	// Newest is the latest time a source observed a fact touching the cluster.
	Newest map[graph.EntityID]time.Time
}

func gather(facts []graph.Fact) Evidence {
	ev := Evidence{
		Members: map[graph.EntityID]map[graph.EntityID]bool{},
		Emails:  map[graph.EntityID]map[string]bool{},
		Newest:  map[graph.EntityID]time.Time{},
	}
	for _, f := range facts {
		for _, s := range f.Sources {
			for _, id := range []graph.EntityID{f.Subject, f.Object} {
				if id != "" && s.ObservedAt.After(ev.Newest[id]) {
					ev.Newest[id] = s.ObservedAt
				}
			}
		}
		switch {
		case f.Predicate == "member_of" && f.Object != "":
			if ev.Members[f.Object] == nil {
				ev.Members[f.Object] = map[graph.EntityID]bool{}
			}
			ev.Members[f.Object][f.Subject] = true
		case f.Predicate == "email" || f.Predicate == "verified_email":
			if s, ok := f.Value.(string); ok && s != "" {
				if ev.Emails[f.Subject] == nil {
					ev.Emails[f.Subject] = map[string]bool{}
				}
				ev.Emails[f.Subject][strings.ToLower(s)] = true
			}
		}
	}
	return ev
}
