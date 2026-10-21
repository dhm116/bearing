package match

import (
	"strings"
	"time"

	"bearing.example/pkg/graph"
)

// TransparentRule is the rule of links the transparent matcher proposes.
const TransparentRule = "matcher/transparent"

// Weights are the transparent matcher's feature weights. The score is their
// weighted sum, capped at 1.
type Weights struct {
	Name, Members, Email float64
}

// evidence is what the facts say about each cluster.
type evidence struct {
	// members maps a cluster to the clusters that are member_of it.
	members map[graph.EntityID]map[graph.EntityID]bool
	// emails maps a cluster to its lower-cased email attributes.
	emails map[graph.EntityID]map[string]bool
	// newest is the latest time a source observed a fact touching the cluster.
	newest map[graph.EntityID]time.Time
}

func gather(facts []graph.Fact) evidence {
	ev := evidence{
		members: map[graph.EntityID]map[graph.EntityID]bool{},
		emails:  map[graph.EntityID]map[string]bool{},
		newest:  map[graph.EntityID]time.Time{},
	}
	for _, f := range facts {
		for _, s := range f.Sources {
			for _, id := range []graph.EntityID{f.Subject, f.Object} {
				if id != "" && s.ObservedAt.After(ev.newest[id]) {
					ev.newest[id] = s.ObservedAt
				}
			}
		}
		switch {
		case f.Predicate == "member_of" && f.Object != "":
			if ev.members[f.Object] == nil {
				ev.members[f.Object] = map[graph.EntityID]bool{}
			}
			ev.members[f.Object][f.Subject] = true
		case f.Predicate == "email":
			if s, ok := f.Value.(string); ok && s != "" {
				if ev.emails[f.Subject] == nil {
					ev.emails[f.Subject] = map[string]bool{}
				}
				ev.emails[f.Subject][strings.ToLower(s)] = true
			}
		}
	}
	return ev
}

// features are the matcher's inputs for one pair, each 0 to 1.
func (ev evidence) features(c Candidate) map[string]float64 {
	f := map[string]float64{
		"name":    jaroWinkler(normalize(c.A.Name), normalize(c.B.Name)),
		"members": jaccard(ev.members[c.A.Cluster], ev.members[c.B.Cluster]),
	}
	for e := range ev.emails[c.A.Cluster] {
		if ev.emails[c.B.Cluster][e] {
			f["email"] = 1
		}
	}
	return f
}

// score is the weighted sum of features, capped at 1.
func (w Weights) score(f map[string]float64) float64 {
	return min(1, w.Name*f["name"]+w.Members*f["members"]+w.Email*f["email"])
}
