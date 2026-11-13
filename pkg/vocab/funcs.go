package vocab

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"text/template"
)

// funcs is a small deterministic subset of Sprig's template functions,
// copied by name and argument order (the piped value comes last, as in
// Sprig). No environment, clock, randomness, UUIDs or keys. Regex functions
// return errors where Sprig has Must variants. Lists are []string only
// (shortcut: add reflection when a rule needs other lists).
var funcs = template.FuncMap{
	"lower":      strings.ToLower,
	"upper":      strings.ToUpper,
	"trim":       strings.TrimSpace,
	"trimPrefix": func(p, s string) string { return strings.TrimPrefix(s, p) },
	"trimSuffix": func(p, s string) string { return strings.TrimSuffix(s, p) },
	"replace":    func(old, repl, s string) string { return strings.ReplaceAll(s, old, repl) },
	"contains":   func(sub, s string) bool { return strings.Contains(s, sub) },
	"hasPrefix":  func(p, s string) bool { return strings.HasPrefix(s, p) },
	"hasSuffix":  func(p, s string) bool { return strings.HasSuffix(s, p) },
	"trunc": func(n int, s string) string {
		if n >= 0 && len(s) > n {
			return s[:n]
		}
		return s
	},
	"quote":      func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` },
	"splitList":  func(sep, s string) []string { return strings.Split(s, sep) },
	"join":       func(sep string, l []string) string { return strings.Join(l, sep) },
	"first":      func(l []string) string { return pick(l, 0) },
	"last":       func(l []string) string { return pick(l, len(l)-1) },
	"default":    func(d, v string) string { return or(v, d) },
	"b64enc":     func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
	"b64dec":     func(s string) (string, error) { b, err := base64.StdEncoding.DecodeString(s); return string(b), err },
	"sha256sum":  func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) },
	"toJson":     func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err },
	"kebabcase":  func(s string) string { return words(s, "-") },
	"snakecase":  func(s string) string { return words(s, "_") },
	"regexMatch": func(re, s string) (bool, error) { return regexp.MatchString(re, s) },
	"regexFind": func(re, s string) (string, error) {
		r, err := regexp.Compile(re)
		if err != nil {
			return "", err
		}
		return r.FindString(s), nil
	},
	"regexReplaceAll": func(re, s, repl string) (string, error) {
		r, err := regexp.Compile(re)
		if err != nil {
			return "", err
		}
		return r.ReplaceAllString(s, repl), nil
	},
}

func pick(l []string, i int) string {
	if i < 0 || i >= len(l) {
		return ""
	}
	return l[i]
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

var (
	camel = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	other = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

// words lowercases s and joins its words (split at case changes and
// punctuation) with sep.
func words(s, sep string) string {
	s = other.ReplaceAllString(camel.ReplaceAllString(s, "${1} ${2}"), sep)
	return strings.ToLower(strings.Trim(s, sep))
}
