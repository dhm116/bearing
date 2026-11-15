package vocab

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// KindOf is the registry kind for a full term name: core terms are today's
// bare kinds (Component), every other term keeps its full name.
func KindOf(full string) model.Kind {
	if t, ok := strings.CutPrefix(full, CoreNamespace+":"); ok {
		return model.Kind(t)
	}
	return model.Kind(full)
}

// InForce reports whether an asserted mapping holds at t.
func (m Mapping) InForce(t time.Time) bool {
	return m.Status == Asserted && !m.ValidFrom.After(t) && (m.ValidTo == nil || m.ValidTo.After(t))
}

// Registry is base plus every term of mods (ADR 13, spike): kinds parented
// by the asserted broader mapping in force at at, relations and attributes
// as namespaced predicates, and each asserted exact_match in force read as
// its core or operator predicate. Names in mappings must be full. A
// mapping must start at a module term and end at a core or operator term
// (no mapping links two adapters' terms). A kind with two parents is
// refused: the registry has single inheritance, and ADR 13 doesn't say
// whether a term may have several broader terms.
func Registry(base *model.Registry, mods []Module, mappings []Mapping, at time.Time) (*model.Registry, error) {
	owner := map[string]string{CoreNamespace: OwnerCore}
	terms := map[string]Term{}
	for _, m := range mods {
		owner[m.Namespace] = m.Owner
		if m.Owner == OwnerCore {
			continue
		}
		for _, t := range m.Terms {
			terms[Full(m.Namespace, t.Name)] = t
		}
	}
	parent := map[string]string{}
	var exact []Mapping
	for _, mp := range mappings {
		if (mp.Predicate != Broader && mp.Predicate != ExactMatch) || !mp.InForce(at) {
			continue
		}
		ons, _, _ := Split(mp.Object)
		t, ok := terms[mp.Subject]
		switch {
		case !ok:
			return nil, fmt.Errorf("mapping %s: subject %s: %w: not a module term", mp.ID, mp.Subject, ErrMapping)
		case owner[ons] != OwnerCore && owner[ons] != OwnerOperator:
			return nil, fmt.Errorf("mapping %s: object %s: %w: must be a core or operator term", mp.ID, mp.Object, ErrMapping)
		case mp.Predicate == ExactMatch:
			exact = append(exact, mp)
			continue
		case t.Is != IsKind:
			return nil, fmt.Errorf("mapping %s: %s: %w: broader relates kinds; use exact_match", mp.ID, mp.Subject, ErrMapping)
		case parent[mp.Subject] != "" && parent[mp.Subject] != mp.Object:
			return nil, fmt.Errorf("mapping %s: %s: %w: already broader than %s (several broader terms are not supported)", mp.ID, mp.Subject, ErrMapping, parent[mp.Subject])
		}
		parent[mp.Subject] = mp.Object
	}
	var defs []model.KindDef
	var preds []model.Predicate
	for _, name := range slices.Sorted(maps.Keys(terms)) {
		t := terms[name]
		ns, _, _ := Split(name)
		if t.Is == IsKind {
			d := model.KindDef{Name: KindOf(name)}
			if p := parent[name]; p != "" {
				d.Parent = KindOf(p)
			}
			defs = append(defs, d)
			continue
		}
		p, err := predicate(ns, name, t)
		if err != nil {
			return nil, fmt.Errorf("vocab: registry: %w", err)
		}
		preds = append(preds, p)
	}
	r, err := base.Extend(defs, preds)
	if err != nil {
		return nil, fmt.Errorf("vocab: registry: %w", err)
	}
	for _, mp := range exact {
		if r, err = r.ExactMatch(string(KindOf(mp.Subject)), string(KindOf(mp.Object))); err != nil {
			return nil, fmt.Errorf("vocab: registry: mapping %s: %w", mp.ID, err)
		}
	}
	return r, nil
}

// predicate is a module's relation or attribute term as the registry holds
// it. A domain or range entry without a namespace is a term of the module's
// own namespace; core: means the core.
func predicate(ns, name string, t Term) (model.Predicate, error) {
	enum := func(prefix, v string, values map[string]int32) (int32, error) {
		n, ok := values[prefix+strings.ToUpper(v)]
		if !ok {
			return 0, fmt.Errorf("term %s: %w: %q", name, ErrModule, v)
		}
		return n, nil
	}
	p := model.Predicate{Name: name, Relation: t.Is == IsRelation}
	card, err := enum("CARDINALITY_", t.Cardinality, modelv1alpha1.Cardinality_value)
	if err != nil {
		return p, err
	}
	conf, err := enum("CONFLICT_POLICY_", t.Conflict, modelv1alpha1.ConflictPolicy_value)
	if err != nil {
		return p, err
	}
	p.Cardinality, p.Conflict = modelv1alpha1.Cardinality(card), modelv1alpha1.ConflictPolicy(conf)
	if !p.Relation {
		typ, err := enum("VALUE_TYPE_", t.Type, modelv1alpha1.ValueType_value)
		if err != nil {
			return p, err
		}
		p.Type = modelv1alpha1.ValueType(typ)
	}
	kinds := func(names []string) []model.Kind {
		if names == nil {
			return nil
		}
		out := make([]model.Kind, len(names))
		for i, n := range names {
			switch tns, term, ok := Split(n); {
			case !ok:
				n = Full(ns, n)
			case tns == "core":
				n = Full(CoreNamespace, term)
			}
			out[i] = KindOf(n)
		}
		return out
	}
	p.Domain, p.Range = kinds(t.Domain), kinds(t.Range)
	return p, nil
}
