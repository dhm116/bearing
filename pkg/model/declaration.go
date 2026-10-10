package model

import (
	"fmt"
	"regexp"
	"strings"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// ValidateDeclaration checks an adapter's declarations against the registry
// (docs/spec/data-model.md, "Declarations" and "Key types"). Checks that
// need other adapters' declarations or source configuration happen at
// configuration apply. It returns a *ValidationError.
func ValidateDeclaration(d *modelv1alpha1.AdapterDeclaration) error {
	c := &checker{}
	c.enums("", d)
	if d.GetName() == "" {
		c.add(codeMalformed, "name", "is required")
	}
	c.namespaceLike("issuer_type", d.GetIssuerType(), true)
	kinds := map[Kind]bool{}
	keyKinds := map[string]Kind{}
	for i, k := range d.GetKinds() {
		kp := fmt.Sprintf("kinds[%d]", i)
		kind := Kind(k.GetKind())
		switch {
		case kind == "":
			c.add(codeMalformed, kp+".kind", "is required")
		case !kind.Valid():
			c.add(codeNotDeclared, kp+".kind", "%q is not a registered kind", clip(string(kind)))
		case kinds[kind]:
			c.add(codeMalformed, kp+".kind", "%q is declared twice", clip(string(kind)))
		}
		kinds[kind] = true
		for j, key := range k.GetKeys() {
			c.keyType(fmt.Sprintf("%s.keys[%d]", kp, j), d.GetIssuerType(), kind, key, keyKinds)
		}
		fields := map[string]bool{}
		for j, f := range k.GetFields() {
			fp := fmt.Sprintf("%s.fields[%d]", kp, j)
			id := f.GetPredicate() + "/" + ShortName(direction(f))
			if fields[id] {
				c.add(codeMalformed, fp, "%q is declared twice", clip(id))
			}
			fields[id] = true
			c.field(fp, kind, f)
		}
		for j, l := range k.GetLinks() {
			lp := fmt.Sprintf("%s.links[%d]", kp, j)
			c.namespaceLike(lp+".issuer_type", l.GetIssuerType(), true)
			c.keyTypeName(lp+".key_type", l.GetKeyType())
		}
	}
	if w := d.GetWebhook(); w != nil {
		c.webhook("webhook", w)
	}
	return c.err()
}

// headerName is an HTTP field name: the token characters of RFC 9110.
var headerName = regexp.MustCompile("^[0-9A-Za-z!#$%&'*+.^_`|~-]{1,128}$")

// MaxSignaturePrefixBytes bounds a declared signature prefix.
const MaxSignaturePrefixBytes = 32

func (c *checker) webhook(path string, w *modelv1alpha1.WebhookSignature) {
	if w.GetScheme() == modelv1alpha1.WebhookScheme_WEBHOOK_SCHEME_UNSPECIFIED {
		c.add(codeMalformed, path+".scheme", "is required")
	}
	if !headerName.MatchString(w.GetSignatureHeader()) {
		c.add(codeMalformed, path+".signature_header", "is required and must be an HTTP header name")
	}
	if p := w.GetSignaturePrefix(); len(p) > MaxSignaturePrefixBytes || strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r >= 0x7f || r == ',' }) {
		c.add(codeMalformed, path+".signature_prefix", "must be at most %d printable ASCII characters without spaces or commas", MaxSignaturePrefixBytes)
	}
	if h := w.GetDeliveryIdHeader(); h != "" {
		switch {
		case !headerName.MatchString(h):
			c.add(codeMalformed, path+".delivery_id_header", "must be an HTTP header name")
		case strings.EqualFold(h, w.GetSignatureHeader()):
			c.add(codeMalformed, path+".delivery_id_header", "must differ from the signature header")
		}
	}
}

func direction(f *modelv1alpha1.FieldDeclaration) modelv1alpha1.Direction {
	if f.GetDirection() == modelv1alpha1.Direction_DIRECTION_UNSPECIFIED {
		return modelv1alpha1.Direction_DIRECTION_OUT
	}
	return f.GetDirection()
}

func (c *checker) namespaceLike(path, s string, required bool) {
	switch {
	case s == "":
		if required {
			c.add(codeMalformed, path, "is required")
		}
	case !namespacePattern.MatchString(s):
		c.add(codeMalformed, path, "%q must be lowercase letters, digits and hyphens", clip(s))
	}
}

func (c *checker) keyTypeName(path, s string) {
	switch {
	case s == "":
		c.add(codeMalformed, path, "is required")
	case !keyTypePattern.MatchString(s):
		c.add(codeMalformed, path, "%q must be lowercase letters, digits, hyphens and underscores", clip(s))
	}
}

func (c *checker) keyType(path, adapterIssuer string, kind Kind, k *modelv1alpha1.KeyTypeDeclaration, keyKinds map[string]Kind) {
	c.namespaceLike(path+".issuer_type", k.GetIssuerType(), false)
	c.keyTypeName(path+".key_type", k.GetKeyType())
	issuer := k.GetIssuerType()
	if issuer == "" {
		issuer = adapterIssuer
	}
	id := issuer + ":" + k.GetKeyType()
	if prev, ok := keyKinds[id]; ok {
		c.add(codeMalformed, path, "key type %s is declared twice (one key type has one kind; first under %s)", id, prev)
	}
	keyKinds[id] = kind
	switch k.GetClass() {
	case modelv1alpha1.KeyClass_KEY_CLASS_UNSPECIFIED:
		c.add(codeMalformed, path+".class", "is required")
	case modelv1alpha1.KeyClass_KEY_CLASS_ID:
		if k.GetPerSubject() != modelv1alpha1.PerSubject_PER_SUBJECT_UNSPECIFIED {
			c.add(codeMalformed, path+".per_subject", "applies to name keys only")
		}
		if k.GetRedirects() {
			c.add(codeMalformed, path+".redirects", "applies to name keys only")
		}
	}
}

func (c *checker) field(path string, kind Kind, f *modelv1alpha1.FieldDeclaration) {
	name := f.GetPredicate()
	reg, registered := LookupPredicate(name)
	in := direction(f) == modelv1alpha1.Direction_DIRECTION_IN
	switch {
	case name == "":
		c.add(codeMalformed, path+".predicate", "is required")
		return
	case IsCorePredicate(name):
		c.add(codeCorePredicate, path+".predicate", "only the core claims %q", clip(name))
		return
	case name == PredicateExists:
		c.add(codeMalformed, path+".predicate", "exists is implicit; don't declare it")
		return
	case registered && reg.Relation:
		if in && kind.Valid() && !reg.InRange(kind) || !in && kind.Valid() && !reg.InDomain(kind) {
			c.add(codeDomainMismatch, path+".predicate", "%q doesn't take a %s with direction %s", clip(name), clip(string(kind)), ShortName(direction(f)))
		}
	case registered:
		if kind.Valid() && !reg.InDomain(kind) {
			c.add(codeDomainMismatch, path+".predicate", "%q is not an attribute of %s", clip(name), clip(string(kind)))
		}
	case !ValidAttributeName(name):
		c.add(codeMalformed, path+".predicate", "%q is not an attribute name", clip(name))
	}
	switch {
	case !registered && f.GetType() == modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED:
		c.add(codeMalformed, path+".type", "is required for an unregistered attribute")
	case registered && f.GetType() != modelv1alpha1.ValueType_VALUE_TYPE_UNSPECIFIED && f.GetType() != reg.Type:
		c.add(codeTypeMismatch, path+".type", "%q is registered as %s", clip(name), ShortName(reg.Type))
	}
	switch {
	case !registered && f.GetCardinality() == modelv1alpha1.Cardinality_CARDINALITY_UNSPECIFIED:
		c.add(codeMalformed, path+".cardinality", "is required for an unregistered attribute")
	case registered && f.GetCardinality() != modelv1alpha1.Cardinality_CARDINALITY_UNSPECIFIED && f.GetCardinality() != reg.Cardinality:
		c.add(codeMalformed, path+".cardinality", "%q is registered as %s", clip(name), ShortName(reg.Cardinality))
	}
	if in && (!registered || !reg.Relation) {
		c.add(codeMalformed, path+".direction", "only relations can be declared in")
	}
	if f.GetMatch() == modelv1alpha1.MatchMethod_MATCH_METHOD_MEMBERS && (name != string(RelMemberOf) || !in) {
		c.add(codeMalformed, path+".match", "members applies only to member_of with direction in")
	}
}
