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
	// MembersSeen and EmailsSeen are when a source last observed the facts
	// behind a cluster's Members and Emails: how a deposit is dated.
	MembersSeen, EmailsSeen map[graph.EntityID]time.Time
}

func gather(facts []graph.Fact) Evidence {
	ev := Evidence{
		Members:     map[graph.EntityID]map[graph.EntityID]bool{},
		Emails:      map[graph.EntityID]map[string]bool{},
		MembersSeen: map[graph.EntityID]time.Time{},
		EmailsSeen:  map[graph.EntityID]time.Time{},
	}
	for _, f := range facts {
		switch {
		case f.Predicate == "member_of" && f.Object != "":
			if ev.Members[f.Object] == nil {
				ev.Members[f.Object] = map[graph.EntityID]bool{}
			}
			ev.Members[f.Object][f.Subject] = true
			ev.MembersSeen[f.Object] = laterOf(ev.MembersSeen[f.Object], lastObserved(f))
		case f.Predicate == "email" || f.Predicate == "verified_email":
			if s, ok := f.Value.(string); ok && s != "" {
				if ev.Emails[f.Subject] == nil {
					ev.Emails[f.Subject] = map[string]bool{}
				}
				ev.Emails[f.Subject][strings.ToLower(s)] = true
				ev.EmailsSeen[f.Subject] = laterOf(ev.EmailsSeen[f.Subject], lastObserved(f))
			}
		}
	}
	return ev
}

func lastObserved(f graph.Fact) time.Time {
	var t time.Time
	for _, s := range f.Sources {
		t = laterOf(t, s.ObservedAt)
	}
	return t
}

// dated is when the evidence behind j was last observed (ADR 7: only new
// observations deposit). Each side is the newest of its record's own
// observations (the name) and of the facts behind the members and email
// features j scored; the pair takes the earlier side, since evidence counts
// only while both records are current. Zero when either side has none, as for
// a deleted record.
func dated(j Judgement, ev Evidence) time.Time {
	side := func(r graph.Record) time.Time {
		if r.LastSeen.IsZero() {
			return time.Time{}
		}
		t := r.LastSeen
		if j.Features["members"] > 0 {
			t = laterOf(t, ev.MembersSeen[r.Cluster])
		}
		if j.Features["email"] > 0 {
			t = laterOf(t, ev.EmailsSeen[r.Cluster])
		}
		return t
	}
	a, b := side(j.Candidate.A), side(j.Candidate.B)
	if a.IsZero() || b.IsZero() {
		return time.Time{}
	}
	if b.Before(a) {
		return b
	}
	return a
}
