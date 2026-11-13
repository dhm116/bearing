// Package vocab is ADR 13's vocabulary as data: namespaced terms published in
// modules by their owners (the core, adapters, the operator), and dated
// mappings and rules between them. Spike code: see spike/vocabulary/PLAN.md.
package vocab

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// CoreNamespace is the core module's namespace; the short name core is
// reserved for it.
const CoreNamespace = "com.dougmccall.bearing.core"

// What a term is.
const (
	IsKind      = "kind"
	IsRelation  = "relation"
	IsAttribute = "attribute"
)

// Module owners (ADR 13: only these three define terms).
const (
	OwnerCore     = "core"
	OwnerAdapter  = "adapter"
	OwnerOperator = "operator"
)

// Mapping predicates (SKOS names, plus derives for rules).
const (
	Broader    = "broader"
	ExactMatch = "exact_match"
	CloseMatch = "close_match"
	Derives    = "derives"
)

// Mapping statuses, as identity links have them.
const (
	Proposed = "proposed"
	Asserted = "asserted"
	Rejected = "rejected"
)

// Errors, wrapped with the subject.
var (
	// ErrModule means a module is malformed.
	ErrModule = errors.New("invalid module")
	// ErrNamespace means a namespace is not a reverse domain, or is reserved.
	ErrNamespace = errors.New("invalid namespace")
	// ErrAmbiguous means a short name matches more than one loaded namespace.
	ErrAmbiguous = errors.New("ambiguous short name")
	// ErrUnknownTerm means a name resolves to no loaded namespace or term.
	ErrUnknownTerm = errors.New("unknown term")
	// ErrMapping means a mapping is malformed or links terms it may not.
	ErrMapping = errors.New("invalid mapping")
)

// Module is the terms one owner publishes in one namespace, at one version.
type Module struct {
	// Owner is core, adapter or operator; set by whoever loads the module.
	Owner     string    `yaml:"-"`
	Namespace string    `yaml:"namespace"`
	Version   string    `yaml:"version"`
	Source    Source    `yaml:"source,omitempty"`
	Terms     []Term    `yaml:"terms"`
	Suggests  []Mapping `yaml:"suggests,omitempty"`
}

// Source is where a module came from: a commit, and how it was checked.
type Source struct {
	Repo       string `yaml:"repo,omitempty"`
	Path       string `yaml:"path,omitempty"`
	Ref        string `yaml:"ref,omitempty"`
	SHA256     string `yaml:"sha256,omitempty"`
	VerifiedBy string `yaml:"-"` // commit, sha256, or build for core and adapters
}

// Term is one kind, relation or attribute. Shapes use the proto enum names
// without their prefix (string, one, set).
type Term struct {
	Name        string       `yaml:"name"`
	Is          string       `yaml:"is"`
	Type        string       `yaml:"type,omitempty"`
	Cardinality string       `yaml:"cardinality,omitempty"`
	Conflict    string       `yaml:"conflict,omitempty"`
	Domain      []string     `yaml:"domain,omitempty"`
	Range       []string     `yaml:"range,omitempty"`
	Deprecated  *Deprecation `yaml:"deprecated,omitempty"`
}

// Shape is what grow-only compares: everything but deprecation, which only
// the owner adds and never removes.
func (t Term) Shape() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s", t.Is, t.Type, t.Cardinality, t.Conflict, strings.Join(t.Domain, ","), strings.Join(t.Range, ","))
}

// Deprecation says what replaces a term, mapping or rule.
type Deprecation struct {
	Since      string   `yaml:"since"`
	ReplacedBy string   `yaml:"replaced_by,omitempty"`
	Consider   []string `yaml:"consider,omitempty"`
	Reason     string   `yaml:"reason,omitempty"`
}

// Mapping is a dated, attributed statement between terms, or a derives rule.
// Names are full once resolved; Parse keeps them as written.
type Mapping struct {
	ID            string     `yaml:"id"`
	Version       int        `yaml:"version,omitempty"`
	Subject       string     `yaml:"subject"`
	Predicate     string     `yaml:"predicate"`
	Object        string     `yaml:"object"`
	Target        *Target    `yaml:"target,omitempty"`
	Guess         bool       `yaml:"guess,omitempty"` // a derives rule that only proposes
	Status        string     `yaml:"status,omitempty"`
	Author        string     `yaml:"author,omitempty"`
	Justification string     `yaml:"justification,omitempty"`
	ValidFrom     time.Time  `yaml:"-"`
	ValidTo       *time.Time `yaml:"-"`
}

// Ref names a mapping version, as fact_source.rule holds it.
func (m Mapping) Ref() string { return fmt.Sprintf("%s@%d", m.ID, max(m.Version, 1)) }

// Target is the object a derives rule makes: its kind and a key template.
type Target struct {
	Kind string `yaml:"kind"`
	Key  string `yaml:"key"`
}

var (
	reverseDomain = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)
	termName      = regexp.MustCompile(`^([A-Z][A-Za-z0-9]*|[a-z][a-z0-9_]*)$`)
)

// Parse reads a module from YAML and checks its own shape.
func Parse(b []byte) (Module, error) {
	var m Module
	if err := yaml.Unmarshal(b, &m); err != nil {
		return Module{}, fmt.Errorf("%w: %w", ErrModule, err)
	}
	return m, m.Check()
}

// Check validates a module on its own: a reverse-domain namespace, unique
// well-formed term names, and a known Is.
func (m Module) Check() error {
	if !reverseDomain.MatchString(m.Namespace) {
		return fmt.Errorf("module %q: %w: want a reverse domain such as io.k8s", m.Namespace, ErrNamespace)
	}
	if m.Version == "" {
		return fmt.Errorf("module %s: %w: no version", m.Namespace, ErrModule)
	}
	seen := map[string]bool{}
	for _, t := range m.Terms {
		switch {
		case !termName.MatchString(t.Name):
			return fmt.Errorf("module %s: term %q: %w: want Kind or snake_case", m.Namespace, t.Name, ErrModule)
		case seen[t.Name]:
			return fmt.Errorf("module %s: term %s: %w: defined twice", m.Namespace, t.Name, ErrModule)
		case !slices.Contains([]string{IsKind, IsRelation, IsAttribute}, t.Is):
			return fmt.Errorf("module %s: term %s: %w: is %q", m.Namespace, t.Name, ErrModule, t.Is)
		}
		seen[t.Name] = true
	}
	return nil
}

// Full is a term's full name.
func Full(namespace, term string) string { return namespace + ":" + term }

// Split is the namespace and term of a full or short name.
func Split(name string) (ns, term string, ok bool) {
	return strings.Cut(name, ":")
}

//go:embed core.yaml
var coreYAML []byte

// Core is the core module, as released with Bearing.
func Core() Module {
	m, err := Parse(coreYAML)
	if err != nil {
		panic(err) // the embedded file is checked by TestCoreModuleMatchesRegistry
	}
	m.Owner, m.Source.VerifiedBy = OwnerCore, "build"
	return m
}
