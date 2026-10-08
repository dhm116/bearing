package resolver

import (
	"strings"
	"unicode"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// keyRef is a key analyzed against the declarations: parsed, case-folded
// where its key type says so, and classified.
type keyRef struct {
	key   model.Key // folded
	ns    string
	kt    string
	id    string
	typ   *keyType
	class modelv1alpha1.KeyClass
}

func (k keyRef) isID() bool   { return k.class == modelv1alpha1.KeyClass_KEY_CLASS_ID }
func (k keyRef) isName() bool { return k.class == modelv1alpha1.KeyClass_KEY_CLASS_NAME }

// groupKey names the family of names a subject holds one of: a namespace
// and key type.
func (k keyRef) groupKey() string { return k.ns + ":" + k.kt }

// lookup parses key and finds its declared key type. It returns false for a
// key that isn't declared for its namespace's issuer type, or whose
// namespace nobody uses.
func (ix *index) lookup(key string) (keyRef, bool) {
	ns, kt, id, err := model.Key(key).Parse()
	if err != nil {
		return keyRef{}, false
	}
	n, ok := ix.namespaces[ns]
	if !ok {
		return keyRef{}, false
	}
	typ, ok := ix.keyTypes[ktID{n.issuerType, kt}]
	if !ok {
		return keyRef{}, false
	}
	class := typ.class
	if c, ok := n.classes[kt]; ok {
		class = c
	}
	if typ.fold {
		id = fold(id)
	}
	return keyRef{key: model.NewKey(ns, kt, id), ns: ns, kt: kt, id: id, typ: typ, class: class}, true
}

// fold applies Unicode simple case folding: runes that SimpleFold puts in
// one orbit compare equal, and each is stored as the smallest member of its
// orbit that isn't upper or title case (so "A" and "a" both become "a").
func fold(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return foldRune(r) != r }) {
		return s
	}
	return strings.Map(foldRune, s)
}

func foldRune(r rune) rune {
	best, found := r, false
	pick := func(c rune) {
		if !unicode.IsUpper(c) && !unicode.IsTitle(c) && (!found || c < best) {
			best, found = c, true
		}
	}
	pick(r)
	for c := unicode.SimpleFold(r); c != r; c = unicode.SimpleFold(c) {
		pick(c)
	}
	if !found {
		return r
	}
	return best
}

// escape percent-encodes "%", "/" and ":" in source-supplied text that goes
// into a state key, so only a subject segment can be a ref
// (docs/spec/contracts.md, "State keys").
func escape(s string) string {
	if !strings.ContainsAny(s, "%/:") {
		return s
	}
	r := strings.NewReplacer("%", "%25", "/", "%2F", ":", "%3A")
	return r.Replace(s)
}
