package testkit

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Leak is one appearance of a secret in scanned data.
type Leak struct {
	// Secret is the index of the secret in the list passed to [FindLeaks].
	Secret int
	// Form is how it was encoded: "raw", "url-query", "url-path", "base64",
	// "base64url", "hex" or "HEX".
	Form string
	// Offset is the byte offset of the first appearance in that form.
	Offset int
}

// minBase64 is the shortest base64 fragment worth matching; shorter ones
// would match by chance.
const minBase64 = 4

// FindLeaks reports where each secret appears in data, raw or in a common
// encoding: URL query or path escaping, standard or URL-safe base64 (at any
// alignment, so a secret inside a larger encoded value such as a Basic auth
// header is still found) and lower- or upper-case hex. It reports the first
// appearance per secret and form. Empty secrets are ignored.
func FindLeaks[T ~string | ~[]byte](data T, secrets ...string) []Leak {
	s := string(data)
	var leaks []Leak
	for i, secret := range secrets {
		if secret == "" {
			continue
		}
		var seen []string
		for _, f := range encodings(secret) {
			for _, p := range f.patterns {
				if len(p) == 0 || slices.Contains(seen, p) {
					continue
				}
				seen = append(seen, p)
				if at := strings.Index(s, p); at >= 0 {
					leaks = append(leaks, Leak{Secret: i, Form: f.name, Offset: at})
					break
				}
			}
		}
	}
	return leaks
}

// AssertNoLeaks fails the test for each leak [FindLeaks] reports, showing the
// form, offset and surrounding text.
func AssertNoLeaks[T ~string | ~[]byte](t testing.TB, data T, secrets ...string) {
	t.Helper()
	s := string(data)
	for _, l := range FindLeaks(s, secrets...) {
		lo, hi := max(0, l.Offset-20), min(len(s), l.Offset+len(secrets[l.Secret])+20)
		t.Errorf("testkit: secret %d leaked (%s) at offset %d: %q", l.Secret, l.Form, l.Offset, s[lo:hi])
	}
}

type encoding struct {
	name     string
	patterns []string
}

func encodings(secret string) []encoding {
	return []encoding{
		{"raw", []string{secret}},
		{"url-query", []string{url.QueryEscape(secret)}},
		{"url-path", []string{url.PathEscape(secret)}},
		{"base64", base64Fragments(base64.RawStdEncoding, secret)},
		{"base64url", base64Fragments(base64.RawURLEncoding, secret)},
		{"hex", []string{hex.EncodeToString([]byte(secret))}},
		{"HEX", []string{strings.ToUpper(hex.EncodeToString([]byte(secret)))}},
	}
}

// base64Fragments returns, for each of the three byte alignments the secret
// can have inside a longer encoded value, the encoded characters that depend
// only on the secret's bytes.
func base64Fragments(enc *base64.Encoding, secret string) []string {
	var out []string
	for shift := range 3 {
		full := enc.EncodeToString([]byte(strings.Repeat("\x00", shift) + secret))
		start := (8*shift + 5) / 6           // first char free of the padding bytes
		end := 8 * (shift + len(secret)) / 6 // chars made only of secret bits
		if end-start >= minBase64 {
			out = append(out, full[start:end])
		}
	}
	return out
}
