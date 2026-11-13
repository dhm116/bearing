package vocab

import (
	"fmt"
	"strings"
	"time"

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

// Registry is base plus every kind term of mods, each parented by the
// asserted broader mapping in force at at, if any. Names in mappings must be
// full. A broader mapping must start at a module kind and end at a core or
// operator kind (ADR 13: no mapping links two adapters' terms); a kind with
// two parents is refused, because the registry has single inheritance.
func Registry(base *model.Registry, mods []Module, mappings []Mapping, at time.Time) (*model.Registry, error) {
	owner := map[string]string{CoreNamespace: OwnerCore}
	kinds := map[string]bool{}
	for _, m := range mods {
		owner[m.Namespace] = m.Owner
		if m.Owner == OwnerCore {
			continue
		}
		for _, t := range m.Terms {
			if t.Is == IsKind {
				kinds[Full(m.Namespace, t.Name)] = true
			}
		}
	}
	parent := map[string]string{}
	for _, mp := range mappings {
		if mp.Predicate != Broader || !mp.InForce(at) {
			continue
		}
		ons, _, _ := Split(mp.Object)
		switch {
		case !kinds[mp.Subject]:
			return nil, fmt.Errorf("mapping %s: subject %s: %w: not a module kind", mp.ID, mp.Subject, ErrMapping)
		case owner[ons] != OwnerCore && owner[ons] != OwnerOperator:
			return nil, fmt.Errorf("mapping %s: object %s: %w: must be a core or operator term", mp.ID, mp.Object, ErrMapping)
		case parent[mp.Subject] != "" && parent[mp.Subject] != mp.Object:
			return nil, fmt.Errorf("mapping %s: %s: %w: already broader than %s", mp.ID, mp.Subject, ErrMapping, parent[mp.Subject])
		}
		parent[mp.Subject] = mp.Object
	}
	defs := make([]model.KindDef, 0, len(kinds))
	for k := range kinds {
		d := model.KindDef{Name: KindOf(k)}
		if p := parent[k]; p != "" {
			d.Parent = KindOf(p)
		}
		defs = append(defs, d)
	}
	r, err := base.Extend(defs, nil)
	if err != nil {
		return nil, fmt.Errorf("vocab: registry: %w", err)
	}
	return r, nil
}
