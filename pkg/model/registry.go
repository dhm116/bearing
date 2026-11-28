package model

import (
	"regexp"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// Kind is a subject kind. Kinds are strings on the wire and come only from
// a Registry: core kinds bare (Component), others by full name
// (io.k8s:Service).
type Kind string

// The core kinds, as pkg/vocab's core module names them.
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

// RelationType is a registered relation predicate.
type RelationType string

// The core relation predicates, as pkg/vocab's core module names them.
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
