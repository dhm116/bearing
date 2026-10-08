package resolver

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// micros converts an instant to the segment algebra's time.
func micros(t time.Time) int64 { return t.UnixMicro() }

// fact is a fact as the resolver writes it: subject and object as IDs or
// refs, after the merges the ChangeSet plans.
type fact struct {
	subject, pred string
	object        *modelv1alpha1.FactObject
	// token names the object in keys: the subject ID of a relation's object,
	// or the hash of an attribute's value.
	token string
}

// newFact builds a fact. The object of a relation is a subject; an
// attribute's value must be canonical.
func newFact(subject, pred string, object *modelv1alpha1.FactObject) (fact, error) {
	f := fact{subject: subject, pred: pred, object: object, token: object.GetSubjectId()}
	if f.token == "" {
		id, err := model.FactID("", pred, object)
		if err != nil {
			return fact{}, fmt.Errorf("fact %s: %w", pred, err)
		}
		f.token = "=" + id
	}
	return f, nil
}

// id identifies the fact among those the resolver writes.
func (f fact) id() string { return f.subject + "\x00" + f.pred + "\x00" + f.token }

// objectSubject returns the subject a relation's object is, or "".
func (f fact) objectSubject() string { return f.object.GetSubjectId() }

// claim is one fact an observation writes, with its valid time and
// confidence after the defaults (docs/spec/data-model.md, "Claims").
type claim struct {
	fact fact
	// in says the observed entity is the fact's object.
	in bool
	// from and to bound an asserted claim; to is posInf when open. For an
	// ending (absent), to is where the fact ended, or posInf for observed_at.
	from, to int64
	absent   bool
	conf     uint32
	// qualifiers are the de-duplicated attributes of the relation claims that
	// made this fact, sorted by their canonical bytes.
	qualifiers []*structpb.Struct
	// object is the other end's key, for the support's via.
	object model.Key
}

// scope is a snapshot scope of the observed entity and one predicate: the
// facts the source claims with the entity as their subject (out) or their
// object (in), or all of them ("*").
type scope struct {
	in   bool
	pred string
	// at is the valid time the scope's watermark starts.
	at int64
	// reason is SNAPSHOT for a scope, DELETED for a deletion.
	reason modelv1alpha1.SupportReason
	// backdate says the scope starts no later than the claims of its predicate
	// do: it is the implicit scope of a `one` predicate.
	backdate bool
}

// claimSet is what an observation writes: the facts it claims and the scopes
// in which it lists every fact.
type claimSet struct {
	claims []*claim
	scopes []scope
}

// claims normalizes the admitted claims of the observation into facts about
// the resolved subjects (docs/spec/data-model.md, "Normalization").
func (u *run) claims(ctx context.Context) (*claimSet, error) {
	p := u.p
	at := micros(p.at)
	e := u.g.mustCanon(ctx, u.chosen)
	entity := p.obs.GetData().GetEntity()
	cs := &claimSet{}
	if entity.GetDeleted() {
		for _, in := range []bool{false, true} {
			cs.scopes = append(cs.scopes, scope{in: in, pred: "*", at: at, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_DELETED})
		}
		return cs, nil
	}
	byID := map[string]*claim{}
	add := func(c *claim) {
		if prev, ok := byID[c.fact.id()]; ok {
			// A fact claimed twice is one claim (the observation passed
			// validation, so the two agree on times and confidence); the
			// qualifiers join.
			prev.qualifiers = mergeQualifiers(prev.qualifiers, c.qualifiers)
			return
		}
		byID[c.fact.id()] = c
		cs.claims = append(cs.claims, c)
	}

	exists, err := newFact(e, model.PredicateExists, &modelv1alpha1.FactObject{Type: modelv1alpha1.ValueType_VALUE_TYPE_BOOL, Value: structpb.NewBoolValue(true)})
	if err != nil {
		return nil, err
	}
	add(&claim{fact: exists, from: at, to: posInf, conf: model.MaxConfidence})

	for _, rc := range p.relations {
		rel := rc.rel
		other := u.g.mustCanon(ctx, u.resolved[rc.other.key])
		subject, object := e, other
		if rc.in {
			subject, object = other, e
		}
		f, err := newFact(subject, rel.GetType(), &modelv1alpha1.FactObject{SubjectId: object})
		if err != nil {
			return nil, err
		}
		c := claimTimes(&claim{fact: f, in: rc.in, object: rc.other.key}, at, rel.GetValidFrom(), rel.GetValidTo(), rel.ConfidencePpm, rel.GetAbsent())
		if q := rel.GetAttributes(); len(q) > 0 {
			c.qualifiers = []*structpb.Struct{{Fields: q}}
		}
		add(c)
		if !rc.in {
			if pred, _ := model.LookupPredicate(rel.GetType()); pred.Cardinality == modelv1alpha1.Cardinality_CARDINALITY_ONE {
				cs.scopes = append(cs.scopes, scope{pred: rel.GetType(), at: at, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT, backdate: true})
			}
		}
	}

	for _, a := range p.attributes {
		values, list := attributeValues(a)
		one := a.shape.Cardinality == modelv1alpha1.Cardinality_CARDINALITY_ONE
		// null or a list is the complete set of the entity's attribute, whatever
		// its cardinality; a single value only for a `one` attribute.
		if list || one {
			cs.scopes = append(cs.scopes, scope{pred: a.pred, at: at, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT, backdate: one})
		}
		for _, v := range values {
			canon, err := model.CanonicalValue(a.shape.Type, v)
			if err != nil {
				return nil, fmt.Errorf("attribute %s: %w", a.pred, err)
			}
			f, err := newFact(e, a.pred, &modelv1alpha1.FactObject{Type: a.shape.Type, Value: canon})
			if err != nil {
				return nil, err
			}
			c := &claim{fact: f}
			if a.claim != nil {
				c = claimTimes(c, at, a.claim.GetValidFrom(), a.claim.GetValidTo(), a.claim.ConfidencePpm, a.claim.GetAbsent())
			} else {
				c = claimTimes(c, at, nil, nil, nil, false)
			}
			add(c)
		}
	}

	for _, snap := range p.obs.GetData().GetSnapshots() {
		for _, pred := range snap.GetPredicates() {
			cs.scopes = append(cs.scopes, scope{
				in: snap.GetDirection() == modelv1alpha1.Direction_DIRECTION_IN, pred: p.storedName(pred), at: at,
				reason: modelv1alpha1.SupportReason_SUPPORT_REASON_SNAPSHOT,
			})
		}
	}
	cs.scopes = p.withoutDropped(cs.scopes)
	cs.scopes = settleScopes(cs.scopes, cs.claims, at)
	return cs, nil
}

// storedName returns the predicate as stored for a name as sent.
func (p *prepared) storedName(name string) string { return p.src.stored(p.kind, name) }

// stored returns a predicate as stored for a name as an adapter sends it for
// a kind: registered names as they are, declared attributes in the source's
// namespace.
func (s *sourceInfo) stored(kind model.Kind, name string) string {
	if _, ok := model.LookupPredicate(name); ok || name == "*" {
		return name
	}
	if _, ok := s.declaredAttributes(kind)[name]; ok {
		return s.reads + "." + name
	}
	return name
}

// withoutDropped removes the predicates of the claims admission rejected from
// the scopes, and the scopes of "*" in a direction a claim was dropped in.
func (p *prepared) withoutDropped(scopes []scope) []scope {
	return slices.DeleteFunc(scopes, func(s scope) bool {
		return slices.ContainsFunc(p.dropped, func(d droppedClaim) bool {
			return d.in == s.in && (s.pred == "*" || s.pred == p.storedName(d.pred))
		})
	})
}

// settleScopes merges scopes of one direction and predicate, and starts those
// of `one` claims no later than the claims do. A scope is the observation's
// statement that it lists every fact; an implicit one for a `one` predicate
// must also end the other objects wherever the claim, which may be
// backdated, takes over (docs/spec/data-model.md, "Supports": at each valid
// time only the source's greatest-key write among all objects counts). A
// deletion's scopes are kept apart: they end by reason.
func settleScopes(scopes []scope, claims []*claim, at int64) []scope {
	type id struct {
		in     bool
		pred   string
		reason modelv1alpha1.SupportReason
	}
	merged := map[id]scope{}
	for _, s := range scopes {
		for _, c := range claims {
			if s.backdate && c.fact.pred == s.pred && c.in == s.in {
				s.at = min(s.at, c.start(at))
			}
		}
		k := id{s.in, s.pred, s.reason}
		if old, ok := merged[k]; ok {
			s.at = min(s.at, old.at)
		}
		merged[k] = s
	}
	out := make([]scope, 0, len(merged))
	for _, s := range merged {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b scope) int {
		switch {
		case a.in != b.in:
			if a.in {
				return 1
			}
			return -1
		case a.pred != b.pred:
			return cmp.Compare(a.pred, b.pred)
		}
		return cmp.Compare(a.reason, b.reason)
	})
	return out
}

// claimTimes applies the defaults to a claim's valid time and confidence.
func claimTimes(c *claim, at int64, from, to *timestamppb.Timestamp, conf *uint32, absent bool) *claim {
	c.from, c.to, c.conf, c.absent = fromTimestamp(from, at), fromTimestamp(to, posInf), model.MaxConfidence, absent
	if conf != nil {
		c.conf = *conf
	}
	return c
}

// start is the valid time the claim first says anything: the start of an
// asserted interval, or the time of an ending.
func (c *claim) start(at int64) int64 {
	switch {
	case c.absent && c.to == posInf:
		return at
	case c.absent || c.to <= c.from:
		return c.to
	}
	return c.from
}

// writes returns the segments the claim writes (docs/spec/data-model.md,
// "Claims"): asserted(conf) on [from, to) and ended from `to` on; an ending
// is ended from its time on.
func (c *claim) writes(at int64, key *resolverv1alpha1.OrderingKey, sup *modelv1alpha1.Support) []seg {
	switch {
	case c.absent:
		end := c.to
		if end == posInf {
			end = at
		}
		return []seg{ending(end, key, modelv1alpha1.SupportReason_SUPPORT_REASON_END)}
	case c.to <= c.from:
		return []seg{ending(c.to, key, modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT)}
	}
	out := []seg{{from: c.from, to: c.to, key: key, live: true, reason: modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT, sup: sup}}
	if c.to != posInf {
		out = append(out, ending(c.to, key, modelv1alpha1.SupportReason_SUPPORT_REASON_ASSERT))
	}
	return out
}

// attributeValues returns the values an attribute claim claims, and whether
// the claim is the complete set (null or a list in entity.attributes).
func attributeValues(a attribute) ([]*structpb.Value, bool) {
	v := a.value
	if a.claim != nil {
		return []*structpb.Value{v}, false
	}
	switch k := v.GetKind().(type) {
	case nil, *structpb.Value_NullValue:
		return nil, true
	case *structpb.Value_ListValue:
		if a.shape.Type != modelv1alpha1.ValueType_VALUE_TYPE_JSON {
			return k.ListValue.GetValues(), true
		}
	}
	return []*structpb.Value{v}, false
}

// mergeQualifiers joins two sets of qualifier objects: de-duplicated and
// sorted by their canonical bytes.
func mergeQualifiers(a, b []*structpb.Struct) []*structpb.Struct {
	type keyed struct {
		bytes []byte
		q     *structpb.Struct
	}
	var all []keyed
	for _, q := range slices.Concat(a, b) {
		bs, err := model.JCS(structpb.NewStructValue(q))
		if err != nil { // validated as finite UTF-8 text
			bs = []byte(q.String())
		}
		all = append(all, keyed{bs, q})
	}
	slices.SortStableFunc(all, func(x, y keyed) int { return bytes.Compare(x.bytes, y.bytes) })
	all = slices.CompactFunc(all, func(x, y keyed) bool { return bytes.Equal(x.bytes, y.bytes) })
	out := make([]*structpb.Struct, len(all))
	for i, k := range all {
		out[i] = proto.CloneOf(k.q)
	}
	return out
}
