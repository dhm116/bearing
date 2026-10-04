package model

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// MaxConfidence is 1.0 in parts per million.
const MaxConfidence = 1_000_000

// MaxFutureSkew is how far observed_at may be after ingest time.
const MaxFutureSkew = 5 * time.Minute

// Limits on google.protobuf.Value trees (attributes, qualifiers, claim
// values), so validating one is linear in its size and bounded.
const (
	// MaxValueDepth is the deepest nesting of lists and objects accepted.
	MaxValueDepth = 32
	// MaxValueEntries is the most elements one list or object may hold.
	MaxValueEntries = 10_000
)

// MaxProblems is how many problems a ValidationError lists; the rest are
// counted.
const MaxProblems = 20

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

// ValidationError lists the problems found, so one pass reports them all.
// It holds at most MaxProblems; More counts the rest.
type ValidationError struct {
	Problems []Problem
	More     int
	// codes holds the distinct codes of every problem, listed or not.
	codes []modelv1alpha1.RejectionCode
}

// Error lists the problems.
func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	s := "invalid: " + strings.Join(parts, "; ")
	if e.More > 0 {
		s += fmt.Sprintf("; and %d more", e.More)
	}
	return s
}

// Has reports whether any problem, listed or not, has code.
func (e *ValidationError) Has(code modelv1alpha1.RejectionCode) bool {
	return slices.Contains(e.Codes(), code)
}

// Codes returns the distinct codes of all problems, in order of first use.
func (e *ValidationError) Codes() []modelv1alpha1.RejectionCode {
	if e.codes != nil {
		return e.codes
	}
	var codes []modelv1alpha1.RejectionCode
	for _, p := range e.Problems {
		if !slices.Contains(codes, p.Code) {
			codes = append(codes, p.Code)
		}
	}
	return codes
}

// checker collects problems.
type checker struct {
	problems []Problem
	more     int
	codes    []modelv1alpha1.RejectionCode
}

// full reports whether the problem list is full, recording code either way.
func (c *checker) full(code modelv1alpha1.RejectionCode) bool {
	if !slices.Contains(c.codes, code) {
		c.codes = append(c.codes, code)
	}
	if len(c.problems) >= MaxProblems {
		c.more++
		return true
	}
	return false
}

func (c *checker) add(code modelv1alpha1.RejectionCode, path, format string, args ...any) {
	if c.full(code) {
		return
	}
	c.problems = append(c.problems, Problem{Code: code, Path: path, Message: fmt.Sprintf(format, args...)})
}

// addAt is add with a lazily formatted path, for deep value trees.
func (c *checker) addAt(code modelv1alpha1.RejectionCode, p *vpath, format string, args ...any) {
	if c.full(code) {
		return
	}
	c.problems = append(c.problems, Problem{Code: code, Path: p.String(), Message: fmt.Sprintf(format, args...)})
}

func (c *checker) err() error {
	if len(c.problems) == 0 && c.more == 0 {
		return nil
	}
	return &ValidationError{Problems: c.problems, More: c.more, codes: c.codes}
}

// vpath is a path into a value tree, formatted only when a problem needs
// it, so walking a deep tree stays linear.
type vpath struct {
	parent *vpath
	base   string // the root's path
	key    string // object member name
	index  int    // list index, when isIndex
	isIdx  bool
}

func rootPath(base string) *vpath { return &vpath{base: base} }

func (p *vpath) member(k string) *vpath { return &vpath{parent: p, key: k} }

func (p *vpath) elem(i int) *vpath { return &vpath{parent: p, index: i, isIdx: true} }

func (p *vpath) String() string {
	if p.parent == nil {
		return p.base
	}
	var segs []*vpath
	for q := p; q.parent != nil; q = q.parent {
		segs = append(segs, q)
	}
	var b strings.Builder
	q := p
	for q.parent != nil {
		q = q.parent
	}
	b.WriteString(q.base)
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i].isIdx {
			b.WriteString("[" + strconv.Itoa(segs[i].index) + "]")
		} else {
			b.WriteString("[" + strconv.Quote(segs[i].key) + "]")
		}
	}
	return b.String()
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
// configuration or state: the envelope, keys, enum values, the registry's
// kinds, relations and attribute types, value trees, confidence, intervals,
// duplicate claims and cardinality within the observation. The core checks
// declarations, namespaces and identity on apply. It returns a
// *ValidationError.
func ValidateObservation(o *eventv1alpha1.Observation) error {
	c := &checker{}
	c.observation(o)
	return c.err()
}

// ValidateAdapterObservation is ValidateObservation for observations an
// adapter returned: it also rejects bearingsource, which only the core sets.
func ValidateAdapterObservation(o *eventv1alpha1.Observation) error {
	c := &checker{}
	if o.GetBearingsource() != "" {
		c.add(codeMalformed, "bearingsource", "is set by the core, never by adapters")
	}
	c.observation(o)
	return c.err()
}

func (c *checker) observation(o *eventv1alpha1.Observation) {
	c.enums("", o)
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
		return
	}
	c.observationData("data", o.GetData(), o.GetTime())
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

// claim is a claim's effective times, confidence and state, with the
// defaults applied, for comparing duplicates and checking cardinality.
type claim struct {
	path     string
	object   string
	from, to int64 // microseconds; to is math.MaxInt64 when open
	conf     uint32
	absent   bool
}

func effective(path, object string, observedAt, from, to *timestamppb.Timestamp, conf *uint32, absent bool) claim {
	cl := claim{path: path, object: object, from: micros(observedAt), to: math.MaxInt64, conf: MaxConfidence, absent: absent}
	if from != nil {
		cl.from = micros(from)
	}
	if to != nil {
		cl.to = micros(to)
	}
	if conf != nil {
		cl.conf = *conf
	}
	return cl
}

func (a claim) sameAs(b claim) bool {
	return a.from == b.from && a.to == b.to && a.conf == b.conf && a.absent == b.absent
}

// asserting reports whether the claim asserts its fact somewhere: not an
// ending, and not "only valid_to at or before observed_at".
func (a claim) asserting() bool { return !a.absent && a.to > a.from }

func micros(ts *timestamppb.Timestamp) int64 {
	if ts == nil {
		return 0
	}
	return ts.AsTime().UnixMicro()
}

func (c *checker) observationData(path string, d *modelv1alpha1.ObservationData, observedAt *timestamppb.Timestamp) {
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

	seenRel := map[string]claim{}
	oneRels := map[string][]claim{}
	for i, r := range d.GetRelations() {
		rp := fmt.Sprintf("%s.relations[%d]", path, i)
		c.relation(rp, kind, r)
		object := r.GetTo() + r.GetFrom()
		cl := effective(rp, object, observedAt, r.GetValidFrom(), r.GetValidTo(), r.ConfidencePpm, r.GetAbsent())
		id := fmt.Sprintf("%s\x00%t\x00%s", r.GetType(), isFrom(r), object)
		if prev, ok := seenRel[id]; ok && !prev.sameAs(cl) {
			c.add(codeDuplicateClaim, rp, "claims the same fact as an earlier relation with different times, confidence or absent")
		}
		seenRel[id] = cl
		if reg, ok := LookupPredicate(r.GetType()); ok && reg.Relation && reg.Cardinality == one && !isFrom(r) && cl.asserting() {
			oneRels[r.GetType()] = append(oneRels[r.GetType()], cl)
		}
	}
	for _, name := range sortedKeys(oneRels) {
		c.overlapping(name, oneRels[name])
	}

	seenAttr := map[string]claim{}
	oneAttrs := map[string][]claim{}
	for i, a := range d.GetAttributeClaims() {
		ap := fmt.Sprintf("%s.attribute_claims[%d]", path, i)
		name := a.GetPredicate()
		if name == "" {
			c.add(codeMalformed, ap+".predicate", "is required")
			continue
		}
		if _, ok := e.GetAttributes()[name]; ok {
			c.add(codeDuplicateClaim, ap, "%q is also in entity.attributes", name)
		}
		if a.GetValue() == nil {
			c.add(codeMalformed, ap+".value", "is required")
			c.attributeName(ap+".predicate", kind, name)
		} else {
			c.attribute(ap+".value", kind, name, a.GetValue(), false)
		}
		c.claimTimes(ap, a.GetValidFrom(), a.GetValidTo(), a.ConfidencePpm)
		valueJSON, err := EncodeJSON(a.GetValue())
		if err != nil {
			continue // already reported as an invalid value
		}
		cl := effective(ap, string(valueJSON), observedAt, a.GetValidFrom(), a.GetValidTo(), a.ConfidencePpm, a.GetAbsent())
		id := name + "\x00" + string(valueJSON)
		if prev, ok := seenAttr[id]; ok && !prev.sameAs(cl) {
			c.add(codeDuplicateClaim, ap, "claims the same fact as an earlier attribute claim with different times, confidence or absent")
		}
		seenAttr[id] = cl
		if reg, ok := LookupPredicate(name); ok && !reg.Relation && reg.Cardinality == one && cl.asserting() {
			oneAttrs[name] = append(oneAttrs[name], cl)
		}
	}
	for _, name := range sortedKeys(oneAttrs) {
		c.overlapping(name, oneAttrs[name])
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

// overlapping rejects asserted claims of different objects of one `one`
// predicate whose valid times overlap. It sorts by start and keeps, for the
// two objects with the latest ends seen so far, their latest end, so it is
// O(n log n).
func (c *checker) overlapping(predicate string, claims []claim) {
	slices.SortStableFunc(claims, func(a, b claim) int {
		switch {
		case a.from < b.from:
			return -1
		case a.from > b.from:
			return 1
		}
		return 0
	})
	type latest struct {
		object string
		end    int64
		set    bool
	}
	var first, second latest
	for _, cl := range claims {
		other := first
		if first.set && first.object == cl.object {
			other = second
		}
		if other.set && other.end > cl.from {
			c.add(codeCardinality, cl.path, "%q takes one value at a time; this claim overlaps another with a different value", predicate)
		}
		switch {
		case first.set && first.object == cl.object:
			first.end = max(first.end, cl.to)
		case second.set && second.object == cl.object:
			second.end = max(second.end, cl.to)
			if second.end > first.end {
				first, second = second, first
			}
		case !first.set || cl.to > first.end:
			second, first = first, latest{cl.object, cl.to, true}
		case !second.set || cl.to > second.end:
			second = latest{cl.object, cl.to, true}
		}
	}
}

func (c *checker) key(path, k string) {
	if k == "" {
		c.add(codeMalformed, path, "is required")
		return
	}
	if !utf8.ValidString(k) {
		c.add(codeMalformed, path, "is not valid UTF-8")
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
		c.tree(rootPath(fmt.Sprintf("%s.attributes[%q]", path, q)), r.GetAttributes()[q], 0)
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
	case name == PredicateExists:
		c.add(codeMalformed, path, "exists is implicit in every observation; don't send it")
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
	if !c.tree(rootPath(path), v, 0) || !registered {
		return
	}
	switch k := v.GetKind().(type) {
	case nil, *structpb.Value_NullValue:
		if !list {
			c.add(codeMalformed, path, "is null; end the fact with absent instead")
		}
	case *structpb.Value_ListValue:
		switch {
		case reg.Type == tJSON:
		case !list && reg.Cardinality == many:
			c.add(codeCardinality, path, "each claim is one value; send one claim per value of %q", name)
		case reg.Cardinality != many:
			c.add(codeCardinality, path, "%q takes one value, not a list", name)
		default:
			for i, e := range k.ListValue.GetValues() {
				c.typedValue(fmt.Sprintf("%s[%d]", path, i), reg.Type, e)
			}
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

// tree checks a value tree: nesting at most MaxValueDepth, at most
// MaxValueEntries per list or object, valid UTF-8, and finite numbers. It
// reports whether the tree passed, and stops descending where it fails.
func (c *checker) tree(p *vpath, v *structpb.Value, depth int) bool {
	if depth > MaxValueDepth {
		c.addAt(codeMalformed, p, "is nested more than %d deep", MaxValueDepth)
		return false
	}
	ok := true
	switch k := v.GetKind().(type) {
	case *structpb.Value_NumberValue:
		if math.IsNaN(k.NumberValue) || math.IsInf(k.NumberValue, 0) {
			c.addAt(codeInvalidValue, p, "number is NaN or infinite")
			ok = false
		}
	case *structpb.Value_StringValue:
		if !utf8.ValidString(k.StringValue) {
			c.addAt(codeMalformed, p, "is not valid UTF-8")
			ok = false
		}
	case *structpb.Value_ListValue:
		values := k.ListValue.GetValues()
		if len(values) > MaxValueEntries {
			c.addAt(codeMalformed, p, "has %d elements, more than %d", len(values), MaxValueEntries)
			return false
		}
		for i, e := range values {
			if !c.tree(p.elem(i), e, depth+1) {
				ok = false
			}
		}
	case *structpb.Value_StructValue:
		fields := k.StructValue.GetFields()
		if len(fields) > MaxValueEntries {
			c.addAt(codeMalformed, p, "has %d members, more than %d", len(fields), MaxValueEntries)
			return false
		}
		for _, name := range sortedKeys(fields) {
			child := p.member(name)
			if !utf8.ValidString(name) {
				c.addAt(codeMalformed, child, "member name is not valid UTF-8")
				ok = false
				continue
			}
			if !c.tree(child, fields[name], depth+1) {
				ok = false
			}
		}
	}
	return ok
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
