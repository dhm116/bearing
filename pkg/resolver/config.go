package resolver

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// Config is what the resolver reads from configuration: the adapter
// declarations in force and the configured sources. It is a plain value
// until configuration resources exist (ADR 10); the resolver for an
// updated configuration is a new one.
type Config struct {
	// Declarations are what the adapters' Describe results declare.
	Declarations []*modelv1alpha1.AdapterDeclaration
	// Sources are the configured adapter instances, by name.
	Sources map[string]*Source
}

// Source is one configured adapter instance, the unit of provenance
// (docs/spec/data-model.md, "Keys and namespaces").
type Source struct {
	// Name is the source's name, for example "github-acme". It may not be
	// "manual" or start with "core/".
	Name string
	// Adapter names the declaration the source runs, for example "github".
	Adapter string
	// Namespace is the namespace the source reads. Default: its adapter's
	// issuer type.
	Namespace string
	// Issues lists other namespaces the source is the issuer for.
	Issues []Namespace
	// Links lists namespaces the source reports linked_ids in.
	Links []Namespace
}

// Namespace is one issuer instance a source issues or links.
type Namespace struct {
	// Name is the namespace, for example "authentik-saml".
	Name string
	// IssuerType is the kind of issuer, for example "saml": the key types of
	// its keys are declared under it.
	IssuerType string
	// KeyClasses overrides the class of a key type in this namespace, by
	// key type: a SAML NameID that isn't persistent is a name.
	KeyClasses map[string]modelv1alpha1.KeyClass
}

// keyType is a declared key type, with the issuer type it is declared under.
type keyType struct {
	issuer     string
	name       string
	kind       model.Kind
	class      modelv1alpha1.KeyClass
	perSubject bool // PER_SUBJECT_ONE
	redirects  bool
	fold       bool // KEY_CASE_INSENSITIVE
}

type ktID struct{ issuer, name string }

// namespace is a namespace some source uses, with its issuer type.
type namespace struct {
	name       string
	issuerType string
	classes    map[string]modelv1alpha1.KeyClass
}

// sourceInfo is a configured source with its declaration looked up.
type sourceInfo struct {
	Source
	decl   *modelv1alpha1.AdapterDeclaration
	reads  string
	issues map[string]bool
	links  map[string]bool
	// kinds holds the declaration of each kind the adapter emits.
	kinds map[model.Kind]*modelv1alpha1.KindDeclaration
}

// index is a validated Config, looked up by key type and namespace.
type index struct {
	keyTypes   map[ktID]*keyType
	namespaces map[string]*namespace
	sources    map[string]*sourceInfo
}

// newIndex validates cfg and builds its lookups. It rejects what the data
// model says configuration apply rejects: two declarations of one key type
// that differ, a namespace used with two issuer types, and an issued or
// linked namespace whose key types nobody declares.
func newIndex(cfg Config) (*index, error) {
	ix := &index{keyTypes: map[ktID]*keyType{}, namespaces: map[string]*namespace{}, sources: map[string]*sourceInfo{}}
	decls := map[string]*modelv1alpha1.AdapterDeclaration{}
	for _, d := range cfg.Declarations {
		if err := model.ValidateDeclaration(d); err != nil {
			return nil, fmt.Errorf("resolver: declaration %q: %w", d.GetName(), err)
		}
		if decls[d.GetName()] != nil {
			return nil, fmt.Errorf("resolver: declaration %q: twice", d.GetName())
		}
		decls[d.GetName()] = d
		for _, kd := range d.GetKinds() {
			for _, k := range kd.GetKeys() {
				kt := &keyType{
					issuer: cmpOr(k.GetIssuerType(), d.GetIssuerType()), name: k.GetKeyType(), kind: model.Kind(kd.GetKind()),
					class: k.GetClass(), perSubject: k.GetPerSubject() == modelv1alpha1.PerSubject_PER_SUBJECT_ONE,
					redirects: k.GetRedirects(), fold: k.GetCase() == modelv1alpha1.KeyCase_KEY_CASE_INSENSITIVE,
				}
				id := ktID{kt.issuer, kt.name}
				if old, ok := ix.keyTypes[id]; ok && *old != *kt {
					return nil, fmt.Errorf("resolver: key type %s:%s is declared twice with different kinds, classes or options", id.issuer, id.name)
				}
				ix.keyTypes[id] = kt
			}
		}
	}
	use := func(name, issuer string, classes map[string]modelv1alpha1.KeyClass) error {
		ns, ok := ix.namespaces[name]
		if !ok {
			ns = &namespace{name: name, issuerType: issuer}
			ix.namespaces[name] = ns
		}
		if ns.issuerType != issuer {
			return fmt.Errorf("resolver: namespace %q is used with issuer types %q and %q", name, ns.issuerType, issuer)
		}
		for kt, class := range classes {
			if old, ok := ns.classes[kt]; ok && old != class {
				return fmt.Errorf("resolver: namespace %q overrides the class of %q twice, differently", name, kt)
			}
			if ns.classes == nil {
				ns.classes = map[string]modelv1alpha1.KeyClass{}
			}
			ns.classes[kt] = class
		}
		return nil
	}
	for name, s := range cfg.Sources {
		if s == nil || s.Name != name {
			return nil, fmt.Errorf("resolver: source %q: the name inside must match", name)
		}
		if name == model.SourceManual || strings.HasPrefix(name, "core/") || strings.Contains(name, "\x00") || name == "" {
			return nil, fmt.Errorf("resolver: source name %q is reserved or invalid", name)
		}
		d, ok := decls[s.Adapter]
		if !ok {
			return nil, fmt.Errorf("resolver: source %q: no declaration for adapter %q", name, s.Adapter)
		}
		si := &sourceInfo{Source: *s, decl: d, issues: map[string]bool{}, links: map[string]bool{}, kinds: map[model.Kind]*modelv1alpha1.KindDeclaration{}}
		si.reads = cmpOr(s.Namespace, d.GetIssuerType())
		if err := use(si.reads, d.GetIssuerType(), nil); err != nil {
			return nil, err
		}
		for _, n := range s.Issues {
			si.issues[n.Name] = true
			if err := use(n.Name, n.IssuerType, n.KeyClasses); err != nil {
				return nil, err
			}
		}
		for _, n := range s.Links {
			si.links[n.Name] = true
			if err := use(n.Name, n.IssuerType, n.KeyClasses); err != nil {
				return nil, err
			}
		}
		for _, kd := range d.GetKinds() {
			si.kinds[model.Kind(kd.GetKind())] = kd
		}
		ix.sources[name] = si
	}
	for _, name := range slices.Sorted(mapKeys(ix.namespaces)) {
		ns := ix.namespaces[name]
		if !slices.ContainsFunc(slices.Collect(mapKeys(ix.keyTypes)), func(id ktID) bool { return id.issuer == ns.issuerType }) {
			return nil, fmt.Errorf("resolver: namespace %q has issuer type %q, which no declaration has key types for", name, ns.issuerType)
		}
	}
	return ix, nil
}

// cmpOr returns a unless it is empty, then b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// errUnknownSource is returned for an event from a source the configuration
// doesn't have: the host's mistake, not a rejection of the observation.
var errUnknownSource = errors.New("resolver: unknown source")
