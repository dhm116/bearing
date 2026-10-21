package match

import (
	"slices"

	"bearing.example/pkg/graph"
)

// nameBlock is the Jaro-Winkler similarity at which two names make a
// candidate pair.
const nameBlock = 0.9

// Candidate is a pair of records worth scoring.
type Candidate struct {
	A, B graph.Record
}

// Block returns candidate pairs of records of one kind in different
// clusters: equal normalized names, Jaro-Winkler of at least 0.9 on names, or
// a shared member (members maps a cluster to the clusters that are
// member_of it). Pairs for which skip is true are left out. The scan is
// quadratic within a kind; upgrade to a name index when kinds hold tens of
// thousands of records.
func Block(recs []graph.Record, members map[graph.EntityID]map[graph.EntityID]bool, skip func(a, b graph.EntityID) bool) []Candidate {
	byKind := map[string][]graph.Record{}
	for _, r := range recs {
		byKind[string(r.Kind)] = append(byKind[string(r.Kind)], r)
	}
	var out []Candidate
	for _, rs := range byKind {
		slices.SortFunc(rs, func(x, y graph.Record) int { return cmpID(x.ID, y.ID) })
		for i, a := range rs {
			for _, b := range rs[i+1:] {
				if a.Cluster == b.Cluster || skip != nil && skip(a.ID, b.ID) {
					continue
				}
				if candidate(a, b, members) {
					out = append(out, Candidate{a, b})
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
	if na != "" && nb != "" && jaroWinkler(na, nb) >= nameBlock {
		return true
	}
	for m := range members[a.Cluster] {
		if members[b.Cluster][m] {
			return true
		}
	}
	return false
}
