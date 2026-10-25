package match

import "context"

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
