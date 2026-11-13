package vocab

import (
	"cmp"
	"maps"
	"slices"
)

// Trail roles: which predicates (as the store holds them) the coverage
// joiner follows. Every field is required; the joiner knows no tool's terms.
type Trail struct {
	// Watches links a monitor to the service it watches; Pages, to the
	// on-call service it notifies.
	Watches, Pages string
	// Describes links a record that configures a monitor (a Terraform
	// resource) to the monitor; the record's claims are the monitor's.
	Describes string
	// DefinedIn links the monitor's configuration to a repository: when that
	// repository is the watched service, a second path to it.
	DefinedIn string
}

// Edge is one open fact the joiner reads, asserted or proposed, with what
// stands behind it.
type Edge struct {
	Fact                  string
	Subject, Object       string // entity IDs
	SubjectKey, ObjectKey string
	Predicate             string
	Proposed              bool
	// ObjectObserved is whether any source observed the object itself; a
	// rule naming a key nobody reports makes an unobserved stub.
	ObjectObserved bool
	Cites          []Cite
}

// Cite is one observation behind a fact and the rule that read it (empty
// for a fact an adapter stated).
type Cite struct{ Observation, Rule string }

// Claim is one thing the sources say about a monitor, with every fact that
// says it. A fact from a record that describes the monitor counts as the
// monitor's own (Via holds the describes facts): the Terraform resource and
// Datadog's API are the intent and outcome of one configuration, so they
// back one claim, not two.
type Claim struct {
	Subject, Predicate, Object string
	Facts, Via                 []Edge
}

// Coverage is a joined trail: Service is covered by Pager, via Monitor.
type Coverage struct {
	Service, Monitor, Pager          string // entity IDs
	ServiceKey, MonitorKey, PagerKey string
	// Reach are the independent claims linking the monitor to the service
	// (watches, defined_in); Page those linking it to the pager.
	Reach, Page []Claim
	// Score is the product of each step's strength, n/(n+1) for n
	// independent claims: ADR 7's Strength with a prior of one, no contrary
	// evidence and no decay. Alternative paths for a step add up; a step
	// every path shares counts once.
	Score    float64
	Features map[string]float64
}

// Break is a trail that stops: the monitor watches a service, but its
// pages step names a record no source observed.
type Break struct {
	Monitor, MonitorKey string
	Step                string // the predicate that resolves nowhere
	Claim               Claim
}

// Join finds every service ← monitor → pager path in edges and scores it,
// in the style of ADR 7's matchers: candidates are the paths, the features
// are claim counts, and the result only proposes. It also reports where a
// trail breaks.
func (t Trail) Join(edges []Edge) ([]Coverage, []Break) {
	describes := map[string]Edge{}
	for _, e := range edges {
		if e.Predicate == t.Describes {
			describes[e.Subject] = e
		}
	}
	type ck struct{ s, p, o string }
	claims := map[ck]*Claim{}
	keys := map[string]string{}
	for _, e := range edges {
		keys[e.Subject], keys[e.Object] = e.SubjectKey, e.ObjectKey
		if e.Predicate != t.Watches && e.Predicate != t.Pages && e.Predicate != t.DefinedIn {
			continue
		}
		subj, via := e.Subject, []Edge(nil)
		if d, ok := describes[e.Subject]; ok {
			subj, via = d.Object, []Edge{d}
		}
		k := ck{subj, e.Predicate, e.Object}
		c := claims[k]
		if c == nil {
			c = &Claim{Subject: subj, Predicate: e.Predicate, Object: e.Object}
			claims[k] = c
		}
		c.Facts = append(c.Facts, e)
		c.Via = append(c.Via, via...)
	}
	// Per monitor: the services it reaches and the pagers it pages.
	reach, page := map[string]map[string][]Claim{}, map[string][]Claim{}
	add := func(m map[string]map[string][]Claim, a, b string, c Claim) {
		if m[a] == nil {
			m[a] = map[string][]Claim{}
		}
		m[a][b] = append(m[a][b], c)
	}
	watched := map[string]bool{}
	for _, k := range slices.SortedFunc(maps.Keys(claims), func(a, b ck) int {
		return cmp.Or(cmp.Compare(a.s, b.s), cmp.Compare(a.p, b.p), cmp.Compare(a.o, b.o))
	}) {
		c := *claims[k]
		switch c.Predicate {
		case t.Watches:
			watched[c.Subject] = true
			add(reach, c.Subject, c.Object, c)
		case t.DefinedIn:
			add(reach, c.Subject, c.Object, c)
		case t.Pages:
			page[c.Subject] = append(page[c.Subject], c)
		}
	}
	var out []Coverage
	var breaks []Break
	for _, m := range slices.Sorted(maps.Keys(watched)) {
		for _, p := range page[m] {
			if !p.Facts[0].ObjectObserved {
				breaks = append(breaks, Break{Monitor: m, MonitorKey: keys[m], Step: t.Pages, Claim: p})
			}
		}
		for _, s := range slices.Sorted(maps.Keys(reach[m])) {
			r := reach[m][s]
			if !slices.ContainsFunc(r, func(c Claim) bool { return c.Predicate == t.Watches }) {
				continue // a repository reference alone says nothing is watched
			}
			for _, p := range page[m] {
				if !p.Facts[0].ObjectObserved {
					continue
				}
				pc := []Claim{p} // shortcut: one claim per pager; add sibling pages paths (e.g. an escalation rule) when a source gives one
				nr, np := float64(len(r)), float64(len(pc))
				out = append(out, Coverage{
					Service: s, Monitor: m, Pager: p.Object,
					ServiceKey: keys[s], MonitorKey: keys[m], PagerKey: keys[p.Object],
					Reach: r, Page: pc,
					Score: nr / (nr + 1) * np / (np + 1),
					Features: map[string]float64{
						"reach_claims": nr, "page_claims": np,
						"observations": float64(len(observations(slices.Concat(r, pc)))),
					},
				})
			}
		}
	}
	return out, breaks
}

// observations are the distinct observations behind cs.
func observations(cs []Claim) map[string]bool {
	out := map[string]bool{}
	for _, c := range cs {
		for _, e := range slices.Concat(c.Facts, c.Via) {
			for _, ct := range e.Cites {
				out[ct.Observation] = true
			}
		}
	}
	return out
}

// Facts are the IDs of every fact on the trail, steps and describes alike,
// sorted and distinct: the proposal's path.
func (c Coverage) Facts() []string {
	var ids []string
	for _, cl := range slices.Concat(c.Reach, c.Page) {
		for _, e := range slices.Concat(cl.Facts, cl.Via) {
			ids = append(ids, e.Fact)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// Observations are the distinct observations behind the trail, sorted.
func (c Coverage) Observations() []string {
	return slices.Sorted(maps.Keys(observations(slices.Concat(c.Reach, c.Page))))
}
