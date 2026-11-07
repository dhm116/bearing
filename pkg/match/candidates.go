package match

import (
	"slices"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// nameSimilar is the Jaro-Winkler similarity at which two names make a
// candidate pair.
const nameSimilar = 0.9

// Candidate is a pair of records worth scoring.
type Candidate struct {
	A, B graph.Record
}

// Candidates is the cheap first pass before scoring (what record linkage
// calls blocking): it returns the pairs of records whose kinds lie on one line
// of descent (reg says so; never kindless), in different clusters, that are worth a matcher's time: equal
// normalized names, Jaro-Winkler of at least 0.9 on names, or a shared
// member (members maps a cluster to the clusters that are member_of it).
// Pairs for which skip is true are left out. The scan is quadratic within a
// kind and between a kind and each of its ancestors; upgrade to a name index when kinds hold tens of thousands of records.
func Candidates(reg *model.Registry, recs []graph.Record, members map[graph.EntityID]map[graph.EntityID]bool, skip func(a, b graph.EntityID) bool) []Candidate {
	byKind := map[string][]graph.Record{}
	for _, r := range recs {
		if r.Kind == "" {
			continue // a relation target no source has described: nothing to match on
		}
		byKind[string(r.Kind)] = append(byKind[string(r.Kind)], r)
	}
	var out []Candidate
	try := func(a, b graph.Record) {
		if cmpID(a.ID, b.ID) > 0 {
			a, b = b, a
		}
		if a.Cluster == b.Cluster || skip != nil && skip(a.ID, b.ID) {
			return
		}
		if candidate(a, b, members) {
			out = append(out, Candidate{a, b})
		}
	}
	for k, rs := range byKind {
		slices.SortFunc(rs, func(x, y graph.Record) int { return cmpID(x.ID, y.ID) })
		for i, a := range rs {
			for _, b := range rs[i+1:] {
				try(a, b)
			}
		}
		for anc := reg.Parent(model.Kind(k)); anc != ""; anc = reg.Parent(anc) {
			for _, a := range byKind[string(anc)] {
				for _, b := range rs {
					try(a, b)
				}
			}
		}
	}
	slices.SortFunc(out, func(x, y Candidate) int {
		if c := cmpID(x.A.ID, y.A.ID); c != 0 {
			return c
		}
		return cmpID(x.B.ID, y.B.ID)
	})
	return out
}

func cmpID(a, b graph.EntityID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func candidate(a, b graph.Record, members map[graph.EntityID]map[graph.EntityID]bool) bool {
	na, nb := normalize(a.Name), normalize(b.Name)
	if na != "" && nb != "" && jaroWinkler(na, nb) >= nameSimilar {
		return true
	}
	for m := range members[a.Cluster] {
		if members[b.Cluster][m] {
			return true
		}
	}
	return false
}
