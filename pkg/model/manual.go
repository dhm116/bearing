package model

import (
	"fmt"
	"regexp"

	"google.golang.org/protobuf/proto"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// SourceManual is the source of manual events.
const SourceManual = "manual"

// subjectIDPattern is a UUIDv7 in canonical lowercase text.
var subjectIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidSubjectID reports whether id is a canonical UUIDv7.
func ValidSubjectID(id string) bool { return subjectIDPattern.MatchString(id) }

// ValidateManualEvent checks the payload of a manual operation
// (docs/spec/data-model.md, "Manual operations") before it is logged: who
// asked and why, subject IDs, keys, fact objects and intervals. Rules that
// need the identity store (already_merged, un-merging a placeholder merge)
// are checked on apply. It returns a *ValidationError.
func ValidateManualEvent(m proto.Message) error {
	c := &checker{}
	switch m := m.(type) {
	case *eventv1alpha1.MergeRequested:
		c.who(m.GetActor(), m.GetReason())
		c.subjectPair("subject_ids", m.GetSubjectIds())
	case *eventv1alpha1.UnmergeRequested:
		c.who(m.GetActor(), m.GetReason())
		c.subjectID("subject_id", m.GetSubjectId())
		if len(m.GetAliases()) == 0 {
			c.add(codeInvalidOperation, "aliases", "must be a non-empty proper subset of the subject's aliases")
		}
		seen := map[string]bool{}
		for i, a := range m.GetAliases() {
			p := fmt.Sprintf("aliases[%d]", i)
			c.key(p, a)
			if seen[a] {
				c.add(codeMalformed, p, "%q is listed twice", a)
			}
			seen[a] = true
		}
	case *eventv1alpha1.DistinctFromSet:
		c.who(m.GetActor(), m.GetReason())
		c.subjectPair("subject_ids", m.GetSubjectIds())
	case *eventv1alpha1.DistinctFromCleared:
		c.who(m.GetActor(), m.GetReason())
		c.subjectPair("subject_ids", m.GetSubjectIds())
	case *eventv1alpha1.ClaimWithdrawn:
		c.who(m.GetActor(), m.GetReason())
		if m.GetSource() == "" {
			c.add(codeMalformed, "source", "is required")
		}
		c.subjectID("subject_id", m.GetSubjectId())
		c.predicate("predicate", m.GetPredicate())
		c.factObject("object", m.GetObject())
	case *eventv1alpha1.OverrideSet:
		c.who(m.GetActor(), m.GetReason())
		c.subjectID("subject_id", m.GetSubjectId())
		c.predicate("predicate", m.GetPredicate())
		for i, o := range m.GetObjects() {
			c.factObject(fmt.Sprintf("objects[%d]", i), o)
		}
		c.claimTimes("", m.GetValidFrom(), m.GetValidTo(), nil)
	case *eventv1alpha1.OverrideCleared:
		c.who(m.GetActor(), m.GetReason())
		c.subjectID("subject_id", m.GetSubjectId())
		c.predicate("predicate", m.GetPredicate())
	case nil:
		c.add(codeMalformed, "", "event is missing")
	default:
		c.add(codeMalformed, "", "%s is not a manual operation", m.ProtoReflect().Descriptor().FullName())
	}
	return c.err()
}

func (c *checker) who(a *eventv1alpha1.Actor, reason string) {
	if a.GetSubject() == "" {
		c.add(codeMalformed, "actor.subject", "is required")
	}
	if reason == "" {
		c.add(codeMalformed, "reason", "is required")
	}
}

func (c *checker) subjectID(path, id string) {
	switch {
	case id == "":
		c.add(codeMalformed, path, "is required")
	case !ValidSubjectID(id):
		c.add(codeMalformed, path, "%q is not a canonical lowercase UUIDv7", id)
	}
}

func (c *checker) subjectPair(path string, ids []string) {
	if len(ids) != 2 {
		c.add(codeMalformed, path, "want exactly two subject IDs, got %d", len(ids))
		return
	}
	for i, id := range ids {
		c.subjectID(fmt.Sprintf("%s[%d]", path, i), id)
	}
	if ids[0] == ids[1] {
		c.add(codeInvalidOperation, path, "names one subject twice")
	}
}

func (c *checker) predicate(path, name string) {
	switch {
	case name == "":
		c.add(codeMalformed, path, "is required")
	case !ValidAttributeName(name) && !validNamespacedAttribute(name):
		c.add(codeMalformed, path, "%q is not a predicate name", name)
	}
}

var namespacedAttribute = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.[a-z][a-z0-9_]*$`)

func validNamespacedAttribute(name string) bool { return namespacedAttribute.MatchString(name) }

func (c *checker) factObject(path string, o *modelv1alpha1.FactObject) {
	if o == nil {
		c.add(codeMalformed, path, "is required")
		return
	}
	if o.GetSubjectId() != "" {
		c.subjectID(path+".subject_id", o.GetSubjectId())
		if o.GetType() != modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED || o.GetValue() != nil {
			c.add(codeMalformed, path, "want subject_id, or type and value, not both")
		}
		return
	}
	if o.GetType() == modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED {
		c.add(codeMalformed, path+".type", "is required for a value")
		return
	}
	if o.GetValue() == nil {
		c.add(codeMalformed, path+".value", "is required")
		return
	}
	c.typedValue(path+".value", o.GetType(), o.GetValue())
}
