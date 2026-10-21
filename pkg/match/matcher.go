package match

import (
	"context"
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

// TransparentRule is the rule of links the transparent matcher proposes.
const TransparentRule = "matcher/transparent"

// Transparent is the built-in matcher: a weighted sum of name similarity
// (Jaro-Winkler), shared members (Jaccard) and a shared email, capped at 1.
// Every feature is kept as evidence, so a proposal shows why.
type Transparent struct {
	// Name, Members and Email weigh the features; zero values take 0.6, 0.3
	// and 0.4.
	Name, Members, Email float64
}

var _ Matcher = Transparent{}

// Rule implements Matcher.
func (Transparent) Rule() string { return TransparentRule }

// Match implements Matcher.
func (t Transparent) Match(_ context.Context, cs []Candidate, ev Evidence) ([]Judgement, error) {
	if t == (Transparent{}) {
		t = Transparent{Name: 0.6, Members: 0.3, Email: 0.4}
	}
	out := make([]Judgement, 0, len(cs))
	for _, c := range cs {
		f := map[string]float64{
			"name":    jaroWinkler(normalize(c.A.Name), normalize(c.B.Name)),
			"members": jaccard(ev.Members[c.A.Cluster], ev.Members[c.B.Cluster]),
		}
		for e := range ev.Emails[c.A.Cluster] {
			if ev.Emails[c.B.Cluster][e] {
				f["email"] = 1
			}
		}
		s := min(1, t.Name*f["name"]+t.Members*f["members"]+t.Email*f["email"])
		out = append(out, Judgement{Candidate: c, Score: s, Features: f})
	}
	return out, nil
}
