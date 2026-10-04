package model

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// MaxConfidence is 1.0 in parts per million.
const MaxConfidence = 1_000_000

// MaxFutureSkew is how far observed_at may be after ingest time.
const MaxFutureSkew = 5 * time.Minute

// Problem is one reason an event, or part of it, is rejected.
type Problem struct {
	Code modelv1alpha1.RejectionCode
	// Path names the field, e.g. "data.relations[0].to".
	Path    string
	Message string
}

// String formats the problem as "path: message (code)".
func (p Problem) String() string {
	return fmt.Sprintf("%s: %s (%s)", p.Path, p.Message, ShortName(p.Code))
}

// ValidationError lists every problem found, so one pass reports them all.
type ValidationError struct {
	Problems []Problem
}

// Error lists every problem.
func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	return "invalid: " + strings.Join(parts, "; ")
}

// Has reports whether any problem has code.
func (e *ValidationError) Has(code modelv1alpha1.RejectionCode) bool {
	return slices.ContainsFunc(e.Problems, func(p Problem) bool { return p.Code == code })
}

// Codes returns the distinct codes of the problems, in order of first use.
func (e *ValidationError) Codes() []modelv1alpha1.RejectionCode {
	var codes []modelv1alpha1.RejectionCode
	for _, p := range e.Problems {
		if !slices.Contains(codes, p.Code) {
			codes = append(codes, p.Code)
		}
	}
	return codes
}

// checker collects problems.
type checker struct{ problems []Problem }

func (c *checker) add(code modelv1alpha1.RejectionCode, path, format string, args ...any) {
	c.problems = append(c.problems, Problem{Code: code, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (c *checker) err() error {
	if len(c.problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: c.problems}
}

const (
	codeMalformed          = modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED
	codeNotDeclared        = modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED
	codeDuplicateClaim     = modelv1alpha1.RejectionCode_REJECTION_CODE_DUPLICATE_CLAIM
	codeCardinality        = modelv1alpha1.RejectionCode_REJECTION_CODE_CARDINALITY_MISMATCH
	codeDomainMismatch     = modelv1alpha1.RejectionCode_REJECTION_CODE_DOMAIN_MISMATCH
	codeTypeMismatch       = modelv1alpha1.RejectionCode_REJECTION_CODE_TYPE_MISMATCH
	codeCorePredicate      = modelv1alpha1.RejectionCode_REJECTION_CODE_CORE_PREDICATE
	codeInvalidValue       = modelv1alpha1.RejectionCode_REJECTION_CODE_INVALID_VALUE
	codeInvalidInterval    = modelv1alpha1.RejectionCode_REJECTION_CODE_INVALID_INTERVAL
	codeInvalidOperation   = modelv1alpha1.RejectionCode_REJECTION_CODE_INVALID_OPERATION
	codeObservedAtInFuture = modelv1alpha1.RejectionCode_REJECTION_CODE_OBSERVED_AT_IN_FUTURE
)

// ValidateObservation checks what can be checked of an observation without
// configuration or state: the envelope, keys, the registry's kinds,
// relations and attribute types, confidence, intervals and duplicate
// claims. The core checks declarations, namespaces and identity on apply.
// It returns a *ValidationError.
func ValidateObservation(o *eventv1alpha1.Observation) error {
	c := &checker{}
	if o.GetSpecversion() != SpecVersion {
		c.add(codeMalformed, "specversion", "must be %q", SpecVersion)
	}
	if o.GetId() == "" {
		c.add(codeMalformed, "id", "is required")
	}
	if o.GetType() != ObservationType {
		c.add(codeMalformed, "type", "must be %q", ObservationType)
	}
	if o.GetSource() == "" {
		c.add(codeMalformed, "source", "is required")
	}
	if o.GetTime() == nil {
		c.add(codeMalformed, "time", "is required")
	} else {
		c.timestamp("time", o.GetTime())
	}
	if o.GetData() == nil {
		c.add(codeMalformed, "data", "is required")
		return c.err()
	}
	c.observationData("data", o.GetData())
	return c.err()
}

// CheckObservedAt rejects an observation whose observed_at is more than
// MaxFutureSkew after ingestedAt.
func CheckObservedAt(o *eventv1alpha1.Observation, ingestedAt time.Time) error {
	if o.GetTime().AsTime().After(ingestedAt.Add(MaxFutureSkew)) {
		return &ValidationError{Problems: []Problem{{
			Code: codeObservedAtInFuture, Path: "time",
			Message: fmt.Sprintf("is more than %s after ingest time %s", MaxFutureSkew, ingestedAt.UTC().Format(time.RFC3339)),
		}}}
	}
	return nil
}

func (c *checker) observationData(path string, d *modelv1alpha1.ObservationData) {
	e := d.GetEntity()
	if e == nil {
		c.add(codeMalformed, path+".entity", "is required")
		return
	}
	ep := path + ".entity"
	kind := Kind(e.GetKind())
	switch {
	case kind == "":
		c.add(codeMalformed, ep+".kind", "is required")
	case !kind.Valid():
		c.add(codeNotDeclared, ep+".kind", "%q is not a registered kind", kind)
	}
	c.key(ep+".key", e.GetKey())
	for i, k := range e.GetAliases() {
		c.key(fmt.Sprintf("%s.aliases[%d]", ep, i), k)
	}
	for i, k := range e.GetLinkedIds() {
		c.key(fmt.Sprintf("%s.linked_ids[%d]", ep, i), k)
	}
	for _, name := range sortedKeys(e.GetAttributes()) {
		c.attribute(fmt.Sprintf("%s.attributes[%q]", ep, name), kind, name, e.GetAttributes()[name], true)
	}

	type claimTimes struct {
		from, to *timestamppb.Timestamp
		conf     *uint32
		absent   bool
	}
	same := func(a, b claimTimes) bool {
		return sameTime(a.from, b.from) && sameTime(a.to, b.to) && equalPtr(a.conf, b.conf) && a.absent == b.absent
	}

	seenRel := map[string]claimTimes{}
	for i, r := range d.GetRelations() {
		rp := fmt.Sprintf("%s.relations[%d]", path, i)
		c.relation(rp, kind, r)
		ct := claimTimes{r.GetValidFrom(), r.GetValidTo(), r.ConfidencePpm, r.GetAbsent()}
		id := fmt.Sprintf("%s\x00%t\x00%s%s", r.GetType(), isFrom(r), r.GetTo(), r.GetFrom())
		if prev, ok := seenRel[id]; ok && !same(prev, ct) {
			c.add(codeDuplicateClaim, rp, "claims the same fact as an earlier relation with different times, confidence or absent")
		}
		seenRel[id] = ct
	}

	seenAttr := map[string]claimTimes{}
	for i, a := range d.GetAttributeClaims() {
		ap := fmt.Sprintf("%s.attribute_claims[%d]", path, i)
		name := a.GetPredicate()
		switch {
		case name == "":
			c.add(codeMalformed, ap+".predicate", "is required")
			continue
		case a.GetValue() == nil && !a.GetAbsent():
			c.add(codeMalformed, ap+".value", "is required unless absent")
		}
		if _, ok := e.GetAttributes()[name]; ok {
			c.add(codeDuplicateClaim, ap, "%q is also in entity.attributes", name)
		}
		if a.GetValue() != nil {
			c.attribute(ap+".value", kind, name, a.GetValue(), false)
		} else {
			c.attributeName(ap+".predicate", kind, name)
		}
		c.claimTimes(ap, a.GetValidFrom(), a.GetValidTo(), a.ConfidencePpm)
		ct := claimTimes{a.GetValidFrom(), a.GetValidTo(), a.ConfidencePpm, a.GetAbsent()}
		valueJSON, _ := a.GetValue().MarshalJSON()
		id := name + "\x00" + string(valueJSON)
		if prev, ok := seenAttr[id]; ok && !same(prev, ct) {
			c.add(codeDuplicateClaim, ap, "claims the same fact as an earlier attribute claim with different times, confidence or absent")
		}
		seenAttr[id] = ct
	}

	for i, s := range d.GetSnapshots() {
		sp := fmt.Sprintf("%s.snapshots[%d]", path, i)
		if s.GetDirection() == modelv1alpha1.Direction_DIRECTION_UNSPECIFIED {
			c.add(codeMalformed, sp+".direction", "is required")
		}
		preds := s.GetPredicates()
		if len(preds) == 0 {
			c.add(codeMalformed, sp+".predicates", "is required")
		}
		for j, p := range preds {
			pp := fmt.Sprintf("%s.predicates[%d]", sp, j)
			switch {
			case p == "*":
				if len(preds) > 1 {
					c.add(codeMalformed, pp, `"*" must be the only predicate`)
				}
			case !ValidAttributeName(p):
				c.add(codeMalformed, pp, "%q is not a predicate name", p)
			case s.GetDirection() == modelv1alpha1.Direction_DIRECTION_IN:
				if reg, ok := LookupPredicate(p); ok && !reg.Relation {
					c.add(codeMalformed, pp, "attribute %q can only be in an out scope", p)
				}
			}
		}
	}
}

func (c *checker) key(path, k string) {
	if k == "" {
		c.add(codeMalformed, path, "is required")
		return
	}
	if _, _, _, err := Key(k).Parse(); err != nil {
		c.add(codeMalformed, path, "%v", err)
	}
}

func (c *checker) timestamp(path string, ts *timestamppb.Timestamp) {
	if err := ts.CheckValid(); err != nil {
		c.add(codeMalformed, path, "%v", err)
	}
}

func (c *checker) claimTimes(path string, from, to *timestamppb.Timestamp, conf *uint32) {
	if from != nil {
		c.timestamp(join(path, "valid_from"), from)
	}
	if to != nil {
		c.timestamp(join(path, "valid_to"), to)
	}
	if from != nil && to != nil && !micro(to).After(micro(from)) {
		c.add(codeInvalidInterval, join(path, "valid_to"), "must be after valid_from")
	}
	if conf != nil && (*conf < 1 || *conf > MaxConfidence) {
		c.add(codeInvalidValue, join(path, "confidence_ppm"), "%d is outside [1, %d]", *conf, MaxConfidence)
	}
}

// join appends a field name to a path, which is empty at the top level.
func join(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}

func (c *checker) relation(path string, entity Kind, r *modelv1alpha1.Relation) {
	name := r.GetType()
	switch end := r.GetEnd().(type) {
	case *modelv1alpha1.Relation_To:
		c.key(path+".to", end.To)
	case *modelv1alpha1.Relation_From:
		c.key(path+".from", end.From)
	default:
		c.add(codeMalformed, path, "needs to or from")
	}
	switch reg, ok := LookupPredicate(name); {
	case name == "":
		c.add(codeMalformed, path+".type", "is required")
	case IsCorePredicate(name):
		c.add(codeCorePredicate, path+".type", "only the core claims %q", name)
	case !ok:
		c.add(codeNotDeclared, path+".type", "%q is not a registered relation", name)
	case !reg.Relation:
		c.add(codeTypeMismatch, path+".type", "%q is an attribute, not a relation", name)
	case entity.Valid() && isFrom(r) && !reg.InRange(entity):
		c.add(codeDomainMismatch, path+".type", "a %s can't be the object of %q", entity, name)
	case entity.Valid() && !isFrom(r) && !reg.InDomain(entity):
		c.add(codeDomainMismatch, path+".type", "a %s can't be the subject of %q", entity, name)
	}
	for _, q := range sortedKeys(r.GetAttributes()) {
		c.finite(fmt.Sprintf("%s.attributes[%q]", path, q), r.GetAttributes()[q])
	}
	c.claimTimes(path, r.GetValidFrom(), r.GetValidTo(), r.ConfidencePpm)
}

func isFrom(r *modelv1alpha1.Relation) bool {
	_, ok := r.GetEnd().(*modelv1alpha1.Relation_From)
	return ok
}

// attributeName checks an attribute's name against the registry and returns
// the registered predicate, if any.
func (c *checker) attributeName(path string, entity Kind, name string) (Predicate, bool) {
	reg, ok := LookupPredicate(name)
	switch {
	case IsCorePredicate(name):
		c.add(codeCorePredicate, path, "only the core claims %q", name)
		return reg, false
	case ok && reg.Relation:
		c.add(codeTypeMismatch, path, "%q is a relation, not an attribute", name)
		return reg, false
	case ok && entity.Valid() && !reg.InDomain(entity):
		c.add(codeDomainMismatch, path, "%q is not an attribute of %s", name, entity)
		return reg, false
	case !ok && !ValidAttributeName(name):
		c.add(codeMalformed, path, "%q is not an attribute name", name)
	}
	return reg, ok
}

// attribute checks one attribute claim. In entity.attributes (list true) a
// list is the complete set of a many attribute and null means none.
func (c *checker) attribute(path string, entity Kind, name string, v *structpb.Value, list bool) {
	reg, registered := c.attributeName(path, entity, name)
	if !registered {
		c.finite(path, v)
		return
	}
	switch k := v.GetKind().(type) {
	case nil, *structpb.Value_NullValue:
		if !list {
			c.add(codeMalformed, path, "is null; end the fact with absent instead")
		}
	case *structpb.Value_ListValue:
		if reg.Type == tJSON {
			c.finite(path, v)
			return
		}
		if !list || reg.Cardinality != many {
			c.add(codeCardinality, path, "%q takes one value, not a list", name)
			return
		}
		for i, e := range k.ListValue.GetValues() {
			c.typedValue(fmt.Sprintf("%s[%d]", path, i), reg.Type, e)
		}
	default:
		c.typedValue(path, reg.Type, v)
	}
}

func (c *checker) typedValue(path string, t modelv1alpha1.ValueType, v *structpb.Value) {
	canon, err := CanonicalValue(t, v)
	switch {
	case errors.Is(err, errTypeMismatch):
		c.add(codeTypeMismatch, path, "want a %s value", ShortName(t))
	case err != nil:
		c.add(codeInvalidValue, path, "%v", err)
	case t == modelv1alpha1.ValueType_VALUE_TYPE_TIME && canon.GetStringValue() != v.GetStringValue():
		c.add(codeInvalidValue, path, "time %q is not canonical; want %q", v.GetStringValue(), canon.GetStringValue())
	}
}

// finite rejects NaN and infinities anywhere in v.
func (c *checker) finite(path string, v *structpb.Value) {
	switch k := v.GetKind().(type) {
	case *structpb.Value_NumberValue:
		if math.IsNaN(k.NumberValue) || math.IsInf(k.NumberValue, 0) {
			c.add(codeInvalidValue, path, "number is NaN or infinite")
		}
	case *structpb.Value_ListValue:
		for i, e := range k.ListValue.GetValues() {
			c.finite(fmt.Sprintf("%s[%d]", path, i), e)
		}
	case *structpb.Value_StructValue:
		fields := k.StructValue.GetFields()
		for _, name := range sortedKeys(fields) {
			c.finite(fmt.Sprintf("%s[%q]", path, name), fields[name])
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func micro(ts *timestamppb.Timestamp) time.Time { return ts.AsTime().Truncate(time.Microsecond) }

func sameTime(a, b *timestamppb.Timestamp) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return micro(a).Equal(micro(b))
}

func equalPtr(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
