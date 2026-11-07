package model

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// Errors Extend returns, wrapped with the subject.
var (
	// ErrKindName means a configured kind's name is malformed or reserved.
	ErrKindName = errors.New("invalid kind name")
	// ErrKindExists means a kind is defined twice.
	ErrKindExists = errors.New("kind already defined")
	// ErrUnknownKind means a parent, domain or range names an undefined kind.
	ErrUnknownKind = errors.New("unknown kind")
	// ErrKindCycle means a kind is its own ancestor.
	ErrKindCycle = errors.New("kind cycle")
	// ErrShapeConflict means a predicate name would have a second shape.
	ErrShapeConflict = errors.New("predicate shape conflict")
	// ErrCoreWiden means a core predicate's domain or range would gain a core kind it doesn't already cover.
	ErrCoreWiden = errors.New("core predicate widened to a core kind")
	// ErrPredicateShape means a new predicate leaves its type, cardinality or conflict policy unset.
	ErrPredicateShape = errors.New("incomplete predicate shape")
	// ErrPredicateName means a predicate name is malformed or reserved.
	ErrPredicateName = errors.New("invalid predicate name")
)

// ReservedNamespace is the namespace of the core kinds, which are spelled bare.
const ReservedNamespace = "bearing"

var configuredKind = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[A-Z][A-Za-z0-9]*$`)

// KindDef is one kind of the vocabulary.
type KindDef struct {
	Name   Kind
	Parent Kind // empty: no parent
	// Retired is when the config dropped the kind; zero means active. Only
	// WithRetired sets it. A retired kind is still valid and still resolves
	// its parent, so facts stored under it keep answering.
	Retired time.Time
}

// core is the core vocabulary, built once; registries are immutable, so
// every Core() call shares it.
var core = buildCore()

// Registry is the vocabulary the server booted with: core kinds and
// predicates, configured kinds, and adapter-declared attribute shapes.
// Immutable after construction, so safe to share. Predicates it returns
// share their Domain and Range slices; callers must not modify them.
type Registry struct {
	kinds    map[Kind]KindDef
	preds    map[string]Predicate
	children map[Kind][]Kind
}

var coreKinds = []Kind{
	KindPerson, KindTeam, KindRepository, KindComponent, KindPackage,
	KindEnvironment, KindCloudResource, KindChange, KindIncident,
	KindSchedule, KindDocument,
}

// Core returns the registry of the core kinds and predicates. A nil
// *Registry reads as Core() everywhere.
func Core() *Registry { return core }

// self is r, or the core vocabulary for a nil r.
func (r *Registry) self() *Registry {
	if r == nil {
		return core
	}
	return r
}

func buildCore() *Registry {
	r := &Registry{kinds: map[Kind]KindDef{}, preds: map[string]Predicate{}}
	for _, k := range coreKinds {
		r.kinds[k] = KindDef{Name: k}
	}
	for _, p := range relations {
		r.preds[p.Name] = p
	}
	exists := attributeShapes[PredicateExists]
	exists.Name = PredicateExists
	r.preds[PredicateExists] = exists
	for _, k := range coreKinds {
		for _, name := range kindAttributes[k] {
			p, ok := r.preds[name]
			if !ok {
				p = attributeShapes[name]
				if p.Type == modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED {
					p = Predicate{Type: tString, Cardinality: one, Conflict: noneC}
				}
				p.Name = name
				p.Domain = []Kind{}
			}
			p.Domain = append(p.Domain, k)
			r.preds[name] = p
		}
	}
	r.index()
	return r
}

func (r *Registry) index() {
	r.children = map[Kind][]Kind{}
	for _, k := range slices.Sorted(maps.Keys(r.kinds)) {
		if p := r.kinds[k].Parent; p != "" {
			r.children[p] = append(r.children[p], k)
		}
	}
}

// Extend returns a registry with kinds and preds added; r is unchanged. A
// predicate that exists must have the same shape, and its domain and range
// merge as a union, though only configured kinds may be added (a nil
// domain or range adds nothing, and one that is nil already stays any kind).
func (r *Registry) Extend(kinds []KindDef, preds []Predicate) (*Registry, error) {
	r = r.self()
	n := &Registry{kinds: maps.Clone(r.kinds), preds: maps.Clone(r.preds)}
	for _, d := range kinds {
		ns, _, _ := strings.Cut(string(d.Name), ".")
		switch {
		case !configuredKind.MatchString(string(d.Name)):
			return nil, fmt.Errorf("kind %q: %w: want <namespace>.<Kind>", clip(string(d.Name)), ErrKindName)
		case ns == ReservedNamespace:
			return nil, fmt.Errorf("kind %q: %w: namespace %s is reserved", clip(string(d.Name)), ErrKindName, ReservedNamespace)
		}
		if _, ok := n.kinds[d.Name]; ok {
			return nil, fmt.Errorf("kind %q: %w", clip(string(d.Name)), ErrKindExists)
		}
		n.kinds[d.Name] = d
	}
	for _, d := range kinds {
		if d.Parent == "" {
			continue
		}
		if _, ok := n.kinds[d.Parent]; !ok {
			return nil, fmt.Errorf("kind %q: parent %q: %w", d.Name, clip(string(d.Parent)), ErrUnknownKind)
		}
		steps := 0
		for k := d.Name; k != ""; k = n.kinds[k].Parent {
			if steps++; steps > len(n.kinds) {
				return nil, fmt.Errorf("kind %q: %w", d.Name, ErrKindCycle)
			}
		}
	}
	for _, p := range preds {
		if err := n.addPredicate(p); err != nil {
			return nil, fmt.Errorf("predicate %q: %w", clip(p.Name), err)
		}
	}
	n.index()
	return n, nil
}

func (r *Registry) addPredicate(p Predicate) error {
	if IsCorePredicate(p.Name) || p.Name == PredicateExists {
		return fmt.Errorf("%w: only the core claims it", ErrPredicateName)
	}
	if !ValidAttributeName(p.Name) {
		return fmt.Errorf("%w: lowercase letters, digits and underscores, no '.'", ErrPredicateName)
	}
	for _, k := range slices.Concat(p.Domain, p.Range) {
		if _, ok := r.kinds[k]; !ok {
			return fmt.Errorf("kind %q: %w", clip(string(k)), ErrUnknownKind)
		}
	}
	old, ok := r.preds[p.Name]
	if !ok {
		if p.Cardinality == modelv1alpha1.Cardinality_CARDINALITY_UNSPECIFIED || p.Conflict == modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_UNSPECIFIED ||
			!p.Relation && p.Type == modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED {
			return fmt.Errorf("%w: cardinality and conflict policy are required, and a type for an attribute", ErrPredicateShape)
		}
		p.Domain, p.Range = slices.Clone(p.Domain), slices.Clone(p.Range)
		r.preds[p.Name] = p
		return nil
	}
	if old.Relation != p.Relation || old.Type != p.Type || old.Cardinality != p.Cardinality || old.Conflict != p.Conflict {
		return fmt.Errorf("%w: it already has another relation flag, type, cardinality or conflict policy", ErrShapeConflict)
	}
	if _, coreOwned := core.preds[p.Name]; coreOwned {
		for _, side := range [][2][]Kind{{old.Domain, p.Domain}, {old.Range, p.Range}} {
			for _, k := range side[1] {
				if _, isCore := core.kinds[k]; isCore && side[0] != nil && !slices.ContainsFunc(side[0], func(d Kind) bool { return r.IsA(k, d) }) {
					return fmt.Errorf("kind %q: %w", k, ErrCoreWiden)
				}
			}
		}
	}
	old.Domain, old.Range = union(old.Domain, p.Domain), union(old.Range, p.Range)
	r.preds[p.Name] = old
	return nil
}

// union merges kind lists; a nil old list means any kind and stays so, and an
// added list that is nil or empty adds nothing.
func union(a, b []Kind) []Kind {
	if a == nil {
		return nil
	}
	out := append(make([]Kind, 0, len(a)+len(b)), a...)
	out = append(out, b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// Valid reports whether k is a kind of the vocabulary.
func (r *Registry) Valid(k Kind) bool { _, ok := r.self().kinds[k]; return ok }

// Retired reports when k was retired, and whether it was.
func (r *Registry) Retired(k Kind) (time.Time, bool) {
	t := r.self().kinds[k].Retired
	return t, !t.IsZero()
}

// WithRetired returns a registry that also holds kinds the config dropped
// (each with Retired set), for reading stored facts; r is unchanged. A
// predicate in preds replaces the domain and range of the one of that name,
// or is added when r has none: the caller passes what the store holds, merged
// with r's. Kinds r already has are left alone.
func (r *Registry) WithRetired(kinds []KindDef, preds []Predicate) *Registry {
	r = r.self()
	n := &Registry{kinds: maps.Clone(r.kinds), preds: maps.Clone(r.preds)}
	for _, d := range kinds {
		if _, ok := n.kinds[d.Name]; !ok {
			n.kinds[d.Name] = d
		}
	}
	for _, p := range preds {
		if old, ok := n.preds[p.Name]; ok {
			old.Domain, old.Range = p.Domain, p.Range
			p = old
		}
		n.preds[p.Name] = p
	}
	n.index()
	return n
}

// Predicates lists every registered predicate, sorted by name.
func (r *Registry) Predicates() []Predicate {
	r = r.self()
	out := make([]Predicate, 0, len(r.preds))
	for _, n := range slices.Sorted(maps.Keys(r.preds)) {
		out = append(out, r.preds[n])
	}
	return out
}

// Kinds lists every kind, sorted.
func (r *Registry) Kinds() []Kind {
	return slices.Sorted(maps.Keys(r.self().kinds))
}

// Parent returns k's parent, or "" when it has none.
func (r *Registry) Parent(k Kind) Kind { return r.self().kinds[k].Parent }

// IsA reports whether k is ancestor or one of its descendants (reflexive).
func (r *Registry) IsA(k, ancestor Kind) bool {
	r = r.self()
	for ; k != ""; k = r.kinds[k].Parent {
		if k == ancestor {
			return true
		}
	}
	return false
}

// Descendants returns k and every kind below it, sorted.
func (r *Registry) Descendants(k Kind) []Kind {
	r = r.self()
	out := []Kind{k}
	for i := 0; i < len(out); i++ {
		out = append(out, r.children[out[i]]...)
	}
	slices.Sort(out)
	return out
}

// WithDescendants returns kinds and every kind below them, sorted, once each.
func (r *Registry) WithDescendants(kinds []Kind) []Kind {
	var out []Kind
	for _, k := range kinds {
		out = append(out, r.Descendants(k)...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Deeper returns the more specific of two kinds on one line of descent; an
// empty kind yields the other.
func (r *Registry) Deeper(a, b Kind) Kind {
	if a == "" || r.IsA(b, a) {
		return b
	}
	return a
}

// OneLine reports whether a and b lie on one line of descent.
func (r *Registry) OneLine(a, b Kind) bool { return r.IsA(a, b) || r.IsA(b, a) }

// Predicate returns a registered predicate. same_as and distinct_from are not
// in the registry; unregistered attributes are adapter-declared.
func (r *Registry) Predicate(name string) (Predicate, bool) {
	p, ok := r.self().preds[name]
	return p, ok
}

// InDomain reports whether k, or an ancestor of k, is in p's domain.
func (r *Registry) InDomain(p Predicate, k Kind) bool { return r.within(p.Domain, k) }

// InRange reports whether k, or an ancestor of k, is in relation p's range.
func (r *Registry) InRange(p Predicate, k Kind) bool { return r.within(p.Range, k) }

func (r *Registry) within(kinds []Kind, k Kind) bool {
	return kinds == nil || slices.ContainsFunc(kinds, func(d Kind) bool { return r.IsA(k, d) })
}
