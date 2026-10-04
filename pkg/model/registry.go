package model

import (
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// Kind is a subject kind. Kinds are strings on the wire and come only from
// the registry in docs/spec/data-model.md ("Subject kinds").
type Kind string

// The registered subject kinds.
const (
	KindPerson        Kind = "Person"
	KindTeam          Kind = "Team"
	KindRepository    Kind = "Repository"
	KindComponent     Kind = "Component"
	KindPackage       Kind = "Package"
	KindEnvironment   Kind = "Environment"
	KindCloudResource Kind = "CloudResource"
	KindChange        Kind = "Change"
	KindIncident      Kind = "Incident"
	KindSchedule      Kind = "Schedule"
	KindDocument      Kind = "Document"
)

// Kinds lists every registered subject kind.
var Kinds = []Kind{
	KindPerson, KindTeam, KindRepository, KindComponent, KindPackage,
	KindEnvironment, KindCloudResource, KindChange, KindIncident,
	KindSchedule, KindDocument,
}

// Valid reports whether k is a registered kind.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// RelationType is a registered relation predicate.
type RelationType string

// The registered relation predicates.
const (
	RelMemberOf        RelationType = "member_of"
	RelOwnedBy         RelationType = "owned_by"
	RelApprovesChanges RelationType = "approves_changes"
	RelDependsOn       RelationType = "depends_on"
	RelPublishes       RelationType = "publishes"
	RelConsumes        RelationType = "consumes"
	RelDeployedTo      RelationType = "deployed_to"
	RelRunsOn          RelationType = "runs_on"
	RelOnCallFor       RelationType = "on_call_for"
	RelAffectedBy      RelationType = "affected_by"
	RelDocuments       RelationType = "documents"
	RelChangedBy       RelationType = "changed_by"
	RelDefinedIn       RelationType = "defined_in"
)

// The core predicates, which adapters must not claim.
const (
	PredicateSameAs       = "same_as"
	PredicateDistinctFrom = "distinct_from"
	PredicateExists       = "exists"
)

// Predicate describes one registered predicate.
type Predicate struct {
	Name string
	// Relation is true for relations, whose objects are subjects.
	Relation bool
	// Domain lists the kinds a subject may have; nil means any kind.
	Domain []Kind
	// Range lists a relation's object kinds; nil means any kind.
	Range []Kind
	// Type is an attribute's value type.
	Type        modelv1alpha1.ValueType
	Cardinality modelv1alpha1.Cardinality
	Conflict    modelv1alpha1.ConflictPolicy
}

// InDomain reports whether k may be the subject of p.
func (p Predicate) InDomain(k Kind) bool { return p.Domain == nil || slices.Contains(p.Domain, k) }

// InRange reports whether k may be the object of relation p.
func (p Predicate) InRange(k Kind) bool { return p.Range == nil || slices.Contains(p.Range, k) }

const (
	one     = modelv1alpha1.Cardinality_CARDINALITY_ONE
	many    = modelv1alpha1.Cardinality_CARDINALITY_MANY
	noneC   = modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE
	oneC    = modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE
	setC    = modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET
	tString = modelv1alpha1.ValueType_VALUE_TYPE_STRING
	tBool   = modelv1alpha1.ValueType_VALUE_TYPE_BOOL
	tTime   = modelv1alpha1.ValueType_VALUE_TYPE_TIME
	tJSON   = modelv1alpha1.ValueType_VALUE_TYPE_JSON
)

func rel(name RelationType, domain, rng []Kind, card modelv1alpha1.Cardinality, conflict modelv1alpha1.ConflictPolicy) Predicate {
	return Predicate{Name: string(name), Relation: true, Domain: domain, Range: rng, Cardinality: card, Conflict: conflict}
}

// relations is the spec's relation table.
var relations = []Predicate{
	rel(RelMemberOf, []Kind{KindPerson, KindTeam}, []Kind{KindTeam}, many, noneC),
	rel(RelOwnedBy, nil, []Kind{KindTeam, KindPerson}, many, setC),
	rel(RelApprovesChanges, []Kind{KindRepository}, []Kind{KindTeam, KindPerson}, many, noneC),
	rel(RelDependsOn, []Kind{KindComponent}, []Kind{KindComponent, KindCloudResource}, many, noneC),
	rel(RelPublishes, []Kind{KindRepository, KindComponent}, []Kind{KindPackage}, many, noneC),
	rel(RelConsumes, []Kind{KindComponent}, []Kind{KindPackage}, many, noneC),
	rel(RelDeployedTo, []Kind{KindComponent, KindCloudResource}, []Kind{KindEnvironment}, many, noneC),
	rel(RelRunsOn, []Kind{KindComponent}, []Kind{KindCloudResource}, many, noneC),
	rel(RelOnCallFor, []Kind{KindSchedule, KindPerson}, []Kind{KindComponent, KindTeam}, many, noneC),
	rel(RelAffectedBy, []Kind{KindComponent}, []Kind{KindIncident}, many, noneC),
	rel(RelDocuments, []Kind{KindDocument}, nil, many, noneC),
	rel(RelChangedBy, []Kind{KindChange}, []Kind{KindPerson}, many, noneC),
	rel(RelDefinedIn, []Kind{KindComponent, KindDocument}, []Kind{KindRepository}, one, oneC),
}

// kindAttributes is the spec's "Registered attributes" column.
var kindAttributes = map[Kind][]string{
	KindPerson:        {"name", "email", "login", "verified_email", "commit_email"},
	KindTeam:          {"name", "slug", "description"},
	KindRepository:    {"name", "full_name", "url", "default_branch", "language", "topics", "archived", "description"},
	KindComponent:     {"type", "tier", "lifecycle"},
	KindPackage:       {"ecosystem", "name", "versions"},
	KindEnvironment:   {"name", "account", "region"},
	KindCloudResource: {"arn", "type", "tags"},
	KindChange:        {"kind", "at", "url"},
	KindIncident:      {"severity", "status", "started_at"},
	KindSchedule:      {"name"},
	KindDocument:      {"title", "url", "doc_type"},
}

// attributeShapes is the spec's attribute table; every other registered
// attribute is a string of cardinality one with conflict none.
var attributeShapes = map[string]Predicate{
	PredicateExists:  {Type: tBool, Cardinality: one, Conflict: noneC},
	"default_branch": {Type: tString, Cardinality: one, Conflict: oneC},
	"archived":       {Type: tBool, Cardinality: one, Conflict: oneC},
	"email":          {Type: tString, Cardinality: many, Conflict: noneC},
	"verified_email": {Type: tString, Cardinality: many, Conflict: noneC},
	"commit_email":   {Type: tString, Cardinality: many, Conflict: noneC},
	"topics":         {Type: tString, Cardinality: many, Conflict: noneC},
	"versions":       {Type: tString, Cardinality: many, Conflict: noneC},
	"tags":           {Type: tJSON, Cardinality: one, Conflict: noneC},
	"at":             {Type: tTime, Cardinality: one, Conflict: noneC},
	"started_at":     {Type: tTime, Cardinality: one, Conflict: noneC},
}

var predicates = buildRegistry()

func buildRegistry() map[string]Predicate {
	reg := map[string]Predicate{}
	for _, p := range relations {
		reg[p.Name] = p
	}
	exists := attributeShapes[PredicateExists]
	exists.Name = PredicateExists
	reg[PredicateExists] = exists
	for _, k := range Kinds {
		for _, name := range kindAttributes[k] {
			p, ok := reg[name]
			if !ok {
				p = attributeShapes[name]
				if p.Type == modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED {
					p = Predicate{Type: tString, Cardinality: one, Conflict: noneC}
				}
				p.Name = name
				p.Domain = []Kind{}
			}
			p.Domain = append(p.Domain, k)
			reg[name] = p
		}
	}
	return reg
}

// LookupPredicate returns a registered predicate. Core predicates (same_as,
// distinct_from) are not in the registry; unregistered attributes are
// adapter-declared.
func LookupPredicate(name string) (Predicate, bool) {
	p, ok := predicates[name]
	return p, ok
}

// IsCorePredicate reports whether only the core may claim name.
func IsCorePredicate(name string) bool {
	return name == PredicateSameAs || name == PredicateDistinctFrom
}

var attributeName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidAttributeName reports whether name is a bare predicate or attribute
// name, as adapters send them (the core adds the namespace).
func ValidAttributeName(name string) bool { return attributeName.MatchString(name) }

// ShortName returns an enum value in the spec's short form: its name without
// the enum's prefix, lowercased (FACT_STATUS_ASSERTED becomes "asserted").
func ShortName(e protoreflect.Enum) string {
	values := e.Descriptor().Values()
	v := values.ByNumber(e.Number())
	if v == nil {
		return ""
	}
	prefix := strings.TrimSuffix(string(values.ByNumber(0).Name()), "UNSPECIFIED")
	return strings.ToLower(strings.TrimPrefix(string(v.Name()), prefix))
}
