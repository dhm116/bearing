// Package model defines Bearing's core schema: the entity kinds, relation
// types and the Observation envelope that adapters emit.
//
// The schema is versioned. Everything in this package is v1; additions are
// backwards compatible, and breaking changes get a new observation type.
package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SpecVersion is the CloudEvents spec version every observation uses.
const SpecVersion = "1.0"

// ObservationType is the CloudEvents type for a v1 observation.
const ObservationType = "dev.bearing.observation.v1"

// Kind is an entity kind in the Bearing schema.
type Kind string

// The v1 entity kinds. docs/spec/data-model.md defines each one.
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

// Kinds lists every v1 entity kind.
var Kinds = []Kind{
	KindPerson, KindTeam, KindRepository, KindComponent, KindPackage,
	KindEnvironment, KindCloudResource, KindChange, KindIncident,
	KindSchedule, KindDocument,
}

// Valid reports whether k is a known v1 kind.
func (k Kind) Valid() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// RelationType is a directed relation from the observed entity to another.
type RelationType string

// The v1 relation types. docs/spec/data-model.md defines each one.
const (
	RelMemberOf   RelationType = "member_of"
	RelOwnedBy    RelationType = "owned_by"
	RelDependsOn  RelationType = "depends_on"
	RelPublishes  RelationType = "publishes"
	RelConsumes   RelationType = "consumes"
	RelDeployedTo RelationType = "deployed_to"
	RelRunsOn     RelationType = "runs_on"
	RelOnCallFor  RelationType = "on_call_for"
	RelAffectedBy RelationType = "affected_by"
	RelDocuments  RelationType = "documents"
	RelChangedBy  RelationType = "changed_by"
	RelDefinedIn  RelationType = "defined_in"
)

// RelationTypes lists every v1 relation type.
var RelationTypes = []RelationType{
	RelMemberOf, RelOwnedBy, RelDependsOn, RelPublishes, RelConsumes,
	RelDeployedTo, RelRunsOn, RelOnCallFor, RelAffectedBy, RelDocuments,
	RelChangedBy, RelDefinedIn,
}

// Valid reports whether r is a known v1 relation type.
func (r RelationType) Valid() bool {
	for _, known := range RelationTypes {
		if r == known {
			return true
		}
	}
	return false
}

// Key identifies an entity inside one source system, in the form
// "<system>:<type>/<id>", for example "github:user/jdoe" or
// "pagerduty:schedule/PX12AB". Keys are never shared across systems; the core
// decides when two keys refer to the same entity.
type Key string

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*:[a-z0-9][a-z0-9_-]*/.+$`)

// Parse splits a key into its system, type and id parts.
func (k Key) Parse() (system, typ, id string, err error) {
	if !keyPattern.MatchString(string(k)) {
		return "", "", "", fmt.Errorf("invalid key %q: want <system>:<type>/<id>", k)
	}
	system, rest, _ := strings.Cut(string(k), ":")
	typ, id, _ = strings.Cut(rest, "/")
	return system, typ, id, nil
}

// System returns the system part of k, such as "github", or "" when k does
// not parse.
func (k Key) System() string {
	system, _, _, _ := k.Parse()
	return system
}

// NewKey builds a key from its parts.
func NewKey(system, typ, id string) Key {
	return Key(system + ":" + typ + "/" + id)
}

// Entity is what an observation says about one thing in a source system.
type Entity struct {
	Kind       Kind           `json:"kind"`
	Key        Key            `json:"key"`
	Attributes map[string]any `json:"attributes,omitempty"`
	// Deleted marks that the source reports this entity no longer exists.
	Deleted bool `json:"deleted,omitempty"`
}

// Relation is a directed edge from the observed entity to another key.
type Relation struct {
	Type       RelationType   `json:"type"`
	To         Key            `json:"to"`
	Attributes map[string]any `json:"attributes,omitempty"`
	// Absent marks that the source reports this relation no longer holds,
	// for example a team membership that was removed.
	Absent bool `json:"absent,omitempty"`
}

// Evidence points back to where the observation came from, so every fact in
// the graph can show its source.
type Evidence struct {
	URL  string `json:"url,omitempty"`
	Ref  string `json:"ref,omitempty"`
	Note string `json:"note,omitempty"`
}

// ObservationData is the payload of an observation.
type ObservationData struct {
	Entity    Entity     `json:"entity"`
	Relations []Relation `json:"relations,omitempty"`
	Evidence  *Evidence  `json:"evidence,omitempty"`
}

// Observation is a CloudEvents 1.0 envelope carrying one adapter report.
type Observation struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Source          string          `json:"source"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype,omitempty"`
	Data            ObservationData `json:"data"`
}

// NewObservation fills in the envelope fields for an observation from source.
// The id is derived from the entity key and time so re-sending the same
// observation is idempotent for consumers that de-duplicate on id.
func NewObservation(source string, at time.Time, data ObservationData) Observation {
	at = at.UTC().Truncate(time.Second)
	return Observation{
		SpecVersion:     SpecVersion,
		ID:              fmt.Sprintf("%s@%s", data.Entity.Key, at.Format(time.RFC3339)),
		Type:            ObservationType,
		Source:          source,
		Time:            at,
		DataContentType: "application/json",
		Data:            data,
	}
}

// Validate checks an observation against the v1 schema rules that JSON Schema
// alone can't express.
func (o Observation) Validate() error {
	var errs []string
	if o.SpecVersion != SpecVersion {
		errs = append(errs, fmt.Sprintf("specversion must be %q", SpecVersion))
	}
	if o.ID == "" {
		errs = append(errs, "id is required")
	}
	if o.Type != ObservationType {
		errs = append(errs, fmt.Sprintf("type must be %q", ObservationType))
	}
	if o.Source == "" {
		errs = append(errs, "source is required")
	}
	if o.Time.IsZero() {
		errs = append(errs, "time is required")
	}
	if !o.Data.Entity.Kind.Valid() {
		errs = append(errs, fmt.Sprintf("unknown entity kind %q", o.Data.Entity.Kind))
	}
	if _, _, _, err := o.Data.Entity.Key.Parse(); err != nil {
		errs = append(errs, err.Error())
	}
	for i, r := range o.Data.Relations {
		if !r.Type.Valid() {
			errs = append(errs, fmt.Sprintf("relations[%d]: unknown type %q", i, r.Type))
		}
		if _, _, _, err := r.To.Parse(); err != nil {
			errs = append(errs, fmt.Sprintf("relations[%d]: %v", i, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid observation: %s", strings.Join(errs, "; "))
	}
	return nil
}

// DecodeObservation parses and validates one JSON observation.
func DecodeObservation(b []byte) (Observation, error) {
	var o Observation
	if err := json.Unmarshal(b, &o); err != nil {
		return o, fmt.Errorf("decode observation: %w", err)
	}
	return o, o.Validate()
}
