package vocab

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// Rule is a compiled derives mapping: for an observation of its subject
// kind, the key template says which entity the object predicate points at.
type Rule struct {
	// ID and Ref name the rule and its version (r-team, r-team@2).
	ID, Ref string
	// Predicate and Kind are the derived fact's predicate and its target's
	// kind, as the store holds them (core terms bare).
	Predicate, Kind string
	// Guess rules only propose.
	Guess   bool
	subject string
	key     *template.Template
}

// Rules are the compiled derives mappings.
type Rules struct{ all []*Rule }

// Compile parses every derives mapping that may run: a template that does
// not parse fails, naming the rule. Proposed, rejected and closed mappings
// are skipped, as are the other predicates.
func Compile(ms []Mapping) (*Rules, error) {
	rs := &Rules{}
	for _, m := range ms {
		if m.Predicate != Derives || m.Status == Proposed || m.Status == Rejected || m.ValidTo != nil {
			continue
		}
		if m.Target == nil || m.Target.Key == "" {
			return nil, fmt.Errorf("rule %s: %w: no target key", m.Ref(), ErrMapping)
		}
		t, err := template.New(m.Ref()).Funcs(funcs).Option("missingkey=error").Parse(m.Target.Key)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w: %w", m.Ref(), ErrMapping, err)
		}
		rs.all = append(rs.all, &Rule{
			ID: m.ID, Ref: m.Ref(), Predicate: string(KindOf(m.Object)), Kind: string(KindOf(m.Target.Kind)),
			Guess: m.Guess, subject: m.Subject, key: t,
		})
	}
	return rs, nil
}

// Match is the rules whose subject is kind.
func (rs *Rules) Match(kind string) []*Rule {
	var out []*Rule
	for _, r := range rs.all {
		if r.subject == kind {
			out = append(out, r)
		}
	}
	return out
}

// Key renders the target key from the observation's entity: its attributes,
// plus key and kind. A missing field or an empty result is no key, not an
// error.
func (r *Rule) Key(kind, key string, attrs map[string]any) (string, error) {
	data := map[string]any{}
	for k, v := range attrs {
		data[k] = v
	}
	data["key"], data["kind"] = key, kind
	var b bytes.Buffer
	if err := r.key.Execute(&b, data); err != nil {
		if strings.Contains(err.Error(), "map has no entry for key") {
			return "", nil
		}
		return "", fmt.Errorf("rule %s: %w", r.Ref, err)
	}
	return strings.TrimSpace(b.String()), nil
}
