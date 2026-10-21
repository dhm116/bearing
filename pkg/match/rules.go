package match

import (
	"fmt"
	"slices"
	"strings"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// KeyRule asserts same_as between records whose keys match two patterns with
// the same {name}, such as A "kubernetes:team/{name}" and B
// "github:team/acme/{name}". The settings name it; the link's rule is
// "rule/<Name>".
type KeyRule struct {
	Name string
	A, B string
}

// capture returns what {name} matched in key, and whether the pattern fits.
func capture(pattern string, key model.Key) (string, bool) {
	prefix, suffix, ok := strings.Cut(pattern, "{name}")
	if !ok {
		return "", false
	}
	k := string(key)
	if len(k) <= len(prefix)+len(suffix) || !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, suffix) {
		return "", false
	}
	return k[len(prefix) : len(k)-len(suffix)], true
}

func (r KeyRule) validate() error {
	if r.Name == "" || !strings.Contains(r.A, "{name}") || !strings.Contains(r.B, "{name}") {
		return fmt.Errorf("rule %q: name and both patterns with {name} are required", r.Name)
	}
	return nil
}

// names returns the {name} values pattern captures from any key of r.
func names(pattern string, r graph.Record) []string {
	var out []string
	for _, k := range r.Keys {
		if n, ok := capture(pattern, k); ok {
			out = append(out, n)
		}
	}
	return out
}

// apply returns the pairs of records, in different clusters, that the rule
// joins, smaller record ID first.
func (r KeyRule) apply(recs []graph.Record) []Candidate {
	var out []Candidate
	for _, a := range recs {
		for _, na := range names(r.A, a) {
			for _, b := range recs {
				if a.ID == b.ID || a.Cluster == b.Cluster || !slices.Contains(names(r.B, b), na) {
					continue
				}
				if b.ID < a.ID {
					out = append(out, Candidate{b, a})
				} else {
					out = append(out, Candidate{a, b})
				}
			}
		}
	}
	return out
}
