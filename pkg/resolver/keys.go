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

// fold applies Unicode simple case folding (CaseFolding.txt, statuses C and
// S): runes that SimpleFold puts in one orbit compare equal, and each is
// stored as the orbit's folded form, which is the lower case of its upper
// case ("A", "a" and "ǅ" become "a" and "ǆ"; "ς" becomes "σ"), except that
// Cherokee folds to upper case.
func fold(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return foldRune(r) != r }) {
		return s
	}
	return strings.Map(foldRune, s)
}

// foldExceptions are the runes whose orbit has no lower case of an upper
// case: CaseFolding.txt maps each to the other member of its pair.
var foldExceptions = map[rune]rune{0x1FD3: 0x0390, 0x1FE3: 0x03B0, 0xFB05: 0xFB06}

func foldRune(r rune) rune {
	if f, ok := foldExceptions[r]; ok {
		return f
	}
	upper := unicode.ToUpper(r)
	if cherokee(upper) {
		return upper
	}
	lower := unicode.ToLower(upper)
	// The simple mapping only counts inside the rune's orbit (İ has none).
	for c := unicode.SimpleFold(r); ; c = unicode.SimpleFold(c) {
		if c == lower {
			return lower
		}
		if c == r {
			return r
		}
	}
}

func cherokee(r rune) bool { return r >= 0x13A0 && r <= 0x13F5 }

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
