package resolver

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	resolverv1alpha1 "bearing.example/gen/go/bearing/resolver/v1alpha1"
	"bearing.example/pkg/model"
)

// Event is one observation to resolve, as the core's event log delivers it.
type Event struct {
	// ID is the event's ID, which includes the source:
	// <source>/<delivery or content ID>. The store applies each ID once.
	ID string
	// Source is the configured source whose event carried the observation:
	// provenance comes from here, never from the CloudEvents source field.
	Source string
	// Observation is the adapter's output. Resolve doesn't change it.
	Observation *eventv1alpha1.Observation
	// Adapter is the adapter's name and version, for example "github@0.3.0",
	// recorded on the supports the event writes. Default: the source's adapter
	// name.
	Adapter string
	// IngestedAt is when the core ingested the event, for the future-time
	// check on observed_at. Zero skips the check.
	IngestedAt time.Time
}

// Rejection is one thing the resolver refused, for the audit log
// (docs/spec/data-model.md, "Audit"). A rejection rejects its scope; the rest
// of the event applies.
type Rejection struct {
	Code  modelv1alpha1.RejectionCode
	Scope model.Scope
	// Path names the field, for example "data.relations[0].to".
	Path    string
	Message string
}

// String describes the rejection for logs and audit messages.
func (r Rejection) String() string {
	return fmt.Sprintf("%s: %s (%s)", r.Path, r.Message, model.ShortName(r.Code))
}

// relation is a relation claim that passed the declaration checks, with its
// other end analyzed.
type relation struct {
	rel *modelv1alpha1.Relation
	// path locates the claim in the observation.
	path string
	// other is the key of the other end, analyzed.
	other keyRef
	// in is true when the entity is the object (the relation is `from`).
	in bool
}

// attribute is an attribute claim that passed the declaration checks: one
// entry of entity.attributes, or one of attribute_claims.
type attribute struct {
	path string
	// name is the attribute as sent; pred is its predicate as stored
	// (<namespace>.<name> when unregistered).
	name, pred string
	shape      model.Predicate
	// claim is nil for an entry of entity.attributes.
	claim *modelv1alpha1.AttributeClaim
	value *structpb.Value
}

// droppedClaim is the direction and predicate of a claim admission rejected.
type droppedClaim struct {
	in   bool
	pred string
}

// prepared is an observation that passed validation and the checks that need
// the declarations, with its keys analyzed.
type prepared struct {
	ev   Event
	src  *sourceInfo
	obs  *eventv1alpha1.Observation
	at   time.Time
	key  *resolverv1alpha1.OrderingKey
	kind model.Kind
	// keys are entity.key first, then entity.aliases: folded, de-duplicated.
	keys []keyRef
	// relations, attributes and links are the claims that were admitted. A
	// deleted entity has none.
	relations  []relation
	attributes []attribute
	links      []keyRef
	// authLinks holds the admitted links that are authoritative evidence: a
	// link declared authoritative, to an id-class key.
	authLinks map[model.Key]bool
	// dropped lists the claims admission rejected, whose predicates leave the
	// snapshot scopes (docs/spec/data-model.md, "Snapshot scopes").
	dropped []droppedClaim
	// rejections are claim-scoped refusals made while preparing.
	rejections []Rejection
}

// prepare validates the event and analyzes its keys. It returns a nil
// *prepared with the rejections when the whole observation is refused.
func (r *Resolver) prepare(ev Event) (*prepared, []Rejection, error) {
	src, ok := r.ix.sources[ev.Source]
	if !ok {
		return nil, nil, fmt.Errorf("%w %q", ErrUnknownSource, ev.Source)
	}
	if ev.ID == "" || ev.Observation == nil {
		return nil, nil, errors.New("resolver: event needs an ID and an observation")
	}
	obs := proto.CloneOf(ev.Observation)
	model.TruncateTimes(obs)
	var rejs []Rejection
	if err := model.ValidateObservationWith(obs, src.declaredAttributes(model.Kind(obs.GetData().GetEntity().GetKind()))); err != nil {
		var ve *model.ValidationError
		if !errors.As(err, &ve) {
			return nil, nil, err
		}
		rejs = append(rejs, problems(ve)...)
		if ve.Scope() == model.ScopeObservation {
			return nil, rejs, nil
		}
		model.DropRejectedClaims(obs, err)
	}
	reject := func(code modelv1alpha1.RejectionCode, path, format string, args ...any) (*prepared, []Rejection, error) {
		return nil, append(rejs, Rejection{Code: code, Scope: model.ScopeObservation, Path: path, Message: fmt.Sprintf(format, args...)}), nil
	}
	if bs := obs.GetBearingsource(); bs != "" && bs != ev.Source {
		return reject(modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED, "bearingsource", "is %q, but the event is from source %q", model.Clip(bs), ev.Source)
	}
	if !ev.IngestedAt.IsZero() {
		if err := model.CheckObservedAt(obs, ev.IngestedAt); err != nil {
			var ve *model.ValidationError
			if errors.As(err, &ve) {
				return nil, append(rejs, problems(ve)...), nil
			}
			return nil, nil, err
		}
	}
	entity := obs.GetData().GetEntity()
	kind := model.Kind(entity.GetKind())
	if src.kinds[kind] == nil {
		return reject(modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED, "data.entity.kind", "adapter %q doesn't declare kind %q", src.Adapter, model.Clip(entity.GetKind()))
	}
	hash, err := model.ContentHash(obs.GetData())
	if err != nil {
		return nil, nil, fmt.Errorf("resolver: content hash: %w", err)
	}
	p := &prepared{ev: ev, src: src, obs: obs, at: obs.GetTime().AsTime(), kind: kind}
	p.key = model.NewOrderingKey(p.at, obs.GetId(), ev.ID, hash)
	entityKeys := append([]string{entity.GetKey()}, entity.GetAliases()...)
	for i, k := range entityKeys {
		path := "data.entity.key"
		if i > 0 {
			path = fmt.Sprintf("data.entity.aliases[%d]", i-1)
		}
		kr, code, msg := r.analyze(src, k, src.entityNamespaces())
		if code != modelv1alpha1.RejectionCode_REJECTION_CODE_UNSPECIFIED {
			return reject(code, path, "%s", msg)
		}
		if kr.typ.kind != kind {
			return reject(modelv1alpha1.RejectionCode_REJECTION_CODE_KIND_MISMATCH, path, "key type %s:%s is declared for kind %s, not %s", kr.ns, kr.kt, kr.typ.kind, kind)
		}
		if !slices.ContainsFunc(p.keys, func(o keyRef) bool { return o.key == kr.key }) {
			p.keys = append(p.keys, kr)
		}
	}
	if !entity.GetDeleted() {
		r.admitClaims(p)
	}
	return p, rejs, nil
}

// problems lists a validation error's problems as rejections.
func problems(ve *model.ValidationError) []Rejection {
	out := make([]Rejection, len(ve.Problems))
	for i, pr := range ve.Problems {
		out[i] = Rejection{Code: pr.Code, Scope: pr.Scope, Path: pr.Path, Message: pr.Message}
	}
	return out
}

// entityNamespaces returns the namespaces an entity's keys may use: the one
// the source reads and those it issues.
func (s *sourceInfo) entityNamespaces() map[string]bool {
	out := map[string]bool{s.reads: true}
	for n := range s.issues {
		out[n] = true
	}
	return out
}

// referenceNamespaces returns the namespaces a reference may use: those of
// an entity's keys, and the ones the source links.
func (s *sourceInfo) referenceNamespaces() map[string]bool {
	out := s.entityNamespaces()
	for n := range s.links {
		out[n] = true
	}
	return out
}

// analyze parses a key and checks it against the namespaces allowed and the
// declared key types. A zero code means the key is fine.
func (r *Resolver) analyze(src *sourceInfo, key string, allowed map[string]bool) (keyRef, modelv1alpha1.RejectionCode, string) {
	ns, _, _, err := model.Key(key).Parse()
	if err != nil {
		return keyRef{}, modelv1alpha1.RejectionCode_REJECTION_CODE_MALFORMED, err.Error()
	}
	if !allowed[ns] {
		return keyRef{}, modelv1alpha1.RejectionCode_REJECTION_CODE_NAMESPACE_NOT_ALLOWED, fmt.Sprintf("source %q doesn't read, issue or link namespace %q here", src.Name, ns)
	}
	kr, ok := r.ix.lookup(key)
	if !ok {
		return keyRef{}, modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED, fmt.Sprintf("key %s has a key type no declaration has for the namespace's issuer type", model.Clip(key))
	}
	return kr, modelv1alpha1.RejectionCode_REJECTION_CODE_UNSPECIFIED, ""
}

// admitClaims checks the claims against the source's declarations and
// analyzes the keys they refer to: the other end of each relation and each
// linked ID. A claim that fails a check is rejected with claim scope and
// left out; the resolver mints no placeholder for it, and its predicate
// leaves the snapshot scopes.
func (r *Resolver) admitClaims(p *prepared) {
	data := p.obs.GetData()
	reject := func(code modelv1alpha1.RejectionCode, path, format string, args ...any) {
		p.rejections = append(p.rejections, Rejection{Code: code, Scope: model.ScopeClaim, Path: path, Message: fmt.Sprintf(format, args...)})
	}
	kd := p.src.kinds[p.kind]
	for i, rel := range data.GetRelations() {
		path := fmt.Sprintf("data.relations[%d]", i)
		in := rel.GetFrom() != ""
		drop := func(code modelv1alpha1.RejectionCode, path, format string, args ...any) {
			reject(code, path, format, args...)
			p.dropped = append(p.dropped, droppedClaim{in: in, pred: rel.GetType()})
		}
		endpoint, field := rel.GetTo(), path+".to"
		dir := modelv1alpha1.Direction_DIRECTION_OUT
		if in {
			endpoint, field, dir = rel.GetFrom(), path+".from", modelv1alpha1.Direction_DIRECTION_IN
		}
		if !declares(kd, rel.GetType(), dir) {
			drop(modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED, path+".type", "adapter %q doesn't declare %s %q for %s", p.src.Adapter, model.ShortName(dir), rel.GetType(), p.kind)
			continue
		}
		other, code, msg := r.analyze(p.src, endpoint, p.src.referenceNamespaces())
		if code != modelv1alpha1.RejectionCode_REJECTION_CODE_UNSPECIFIED {
			drop(code, field, "%s", msg)
			continue
		}
		pred, _ := model.LookupPredicate(rel.GetType())
		subject, object := p.kind, other.typ.kind
		if in {
			subject, object = object, subject
		}
		if !pred.InDomain(subject) || !pred.InRange(object) {
			drop(modelv1alpha1.RejectionCode_REJECTION_CODE_DOMAIN_MISMATCH, path, "%s from %s to %s is outside its domain or range", rel.GetType(), subject, object)
			continue
		}
		p.relations = append(p.relations, relation{rel: rel, path: path, other: other, in: in})
	}
	for _, name := range slices.Sorted(maps.Keys(data.GetEntity().GetAttributes())) {
		p.admitAttribute(fmt.Sprintf("data.entity.attributes[%q]", model.Clip(name)), name, nil, data.GetEntity().GetAttributes()[name], reject)
	}
	for i, ac := range data.GetAttributeClaims() {
		p.admitAttribute(fmt.Sprintf("data.attribute_claims[%d]", i), ac.GetPredicate(), ac, ac.GetValue(), reject)
	}
	for i, l := range p.obs.GetData().GetEntity().GetLinkedIds() {
		path := fmt.Sprintf("data.entity.linked_ids[%d]", i)
		kr, code, msg := r.analyze(p.src, l, p.src.links)
		if code != modelv1alpha1.RejectionCode_REJECTION_CODE_UNSPECIFIED {
			reject(code, path, "%s", msg)
			continue
		}
		issuer := r.ix.namespaces[kr.ns].issuerType
		if !slices.ContainsFunc(kd.GetLinks(), func(ld *modelv1alpha1.LinkDeclaration) bool {
			return ld.GetIssuerType() == issuer && ld.GetKeyType() == kr.kt
		}) || kr.typ.kind != p.kind {
			reject(modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED, path, "adapter %q doesn't declare a link to %s:%s for %s", p.src.Adapter, issuer, kr.kt, p.kind)
			continue
		}
		p.links = append(p.links, kr)
		if kr.isID() && slices.ContainsFunc(kd.GetLinks(), func(ld *modelv1alpha1.LinkDeclaration) bool {
			return ld.GetIssuerType() == issuer && ld.GetKeyType() == kr.kt && ld.GetAuthority().GetAuthoritative()
		}) {
			if p.authLinks == nil {
				p.authLinks = map[model.Key]bool{}
			}
			p.authLinks[kr.key] = true
		}
	}
}

// declares reports whether the kind's declaration has a field for the
// predicate and direction.
func declares(kd *modelv1alpha1.KindDeclaration, pred string, dir modelv1alpha1.Direction) bool {
	return slices.ContainsFunc(kd.GetFields(), func(f *modelv1alpha1.FieldDeclaration) bool {
		return f.GetPredicate() == pred && fieldDirection(f) == dir
	})
}

// admitAttribute checks one attribute claim against the declarations and
// keeps it, or rejects it with claim scope.
func (p *prepared) admitAttribute(path, name string, claim *modelv1alpha1.AttributeClaim, value *structpb.Value, reject func(modelv1alpha1.RejectionCode, string, string, ...any)) {
	if !declares(p.src.kinds[p.kind], name, modelv1alpha1.Direction_DIRECTION_OUT) {
		reject(modelv1alpha1.RejectionCode_REJECTION_CODE_NOT_DECLARED, path, "adapter %q doesn't declare attribute %q for %s", p.src.Adapter, model.Clip(name), p.kind)
		p.dropped = append(p.dropped, droppedClaim{pred: name})
		return
	}
	a := attribute{path: path, name: name, pred: name, claim: claim, value: value}
	if shape, ok := model.LookupPredicate(name); ok {
		a.shape = shape
	} else {
		a.shape = p.src.declaredAttributes(p.kind)[name]
		a.pred = p.src.reads + "." + name
		a.shape.Name = a.pred
	}
	p.attributes = append(p.attributes, a)
}

// declaredAttributes returns the shapes of the unregistered attributes the
// source declares for a kind.
func (s *sourceInfo) declaredAttributes(kind model.Kind) map[string]model.Predicate {
	var out map[string]model.Predicate
	for _, f := range s.kinds[kind].GetFields() {
		if _, registered := model.LookupPredicate(f.GetPredicate()); registered || fieldDirection(f) != modelv1alpha1.Direction_DIRECTION_OUT {
			continue
		}
		if out == nil {
			out = map[string]model.Predicate{}
		}
		out[f.GetPredicate()] = model.Predicate{
			Name: f.GetPredicate(), Type: f.GetType(), Cardinality: f.GetCardinality(), Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE,
		}
	}
	return out
}

func fieldDirection(f *modelv1alpha1.FieldDeclaration) modelv1alpha1.Direction {
	if f.GetDirection() == modelv1alpha1.Direction_DIRECTION_UNSPECIFIED {
		return modelv1alpha1.Direction_DIRECTION_OUT
	}
	return f.GetDirection()
}
