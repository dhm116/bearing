package testkit

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Leak is one appearance of a secret in scanned data.
type Leak struct {
	// Secret is the index of the secret in the list passed to [FindLeaks].
	Secret int
	// Form is how it was encoded: "raw", "url-query", "url-path",
	// "url-escaped-path", "json", "quoted", "base64", "base64url", "hex" or
	// "HEX". The URL forms include lower-case percent escapes.
	Form string
	// Offset is the byte offset of the first appearance in that form.
	Offset int
}

// minBase64 is the shortest base64 fragment worth matching; shorter ones
// would match by chance.
const minBase64 = 4

// FindLeaks reports where each secret appears in data, raw or in a common
// encoding: URL query escaping, path-segment escaping and url.URL's escaped
// path (as in an url.full span attribute), each with upper- or lower-case
// percent escapes; a JSON string body, with or without HTML escaping (as
// slog's JSON handler writes it); a Go-quoted string body (as slog's text
// handler writes it); standard or URL-safe base64 at any alignment, so a
// secret inside a larger encoded value such as a Basic auth header is
// still found; and lower- or upper-case hex. It reports the first
// appearance per secret and form. Empty secrets are ignored.
//
// Base64 is matched only on characters that depend on the secret alone,
// and only when there are at least four of them: secrets of four bytes or
// more are found at every alignment, three-byte secrets only at aligned
// offsets, and one- or two-byte secrets not at all in base64. Canaries from
// [Canary] are long enough.
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
	for i, secret := range secrets {
		if secret != "" && len(secret) < 4 {
			t.Logf("testkit: secret %d is shorter than 4 bytes; base64 forms are only partly checked", i)
		}
	}
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
		{"url-query", withLowerEscapes(url.QueryEscape(secret))},
		{"url-path", withLowerEscapes(url.PathEscape(secret))},
		{"url-escaped-path", withLowerEscapes((&url.URL{Path: secret}).EscapedPath())},
		{"json", jsonBodies(secret)},
		{"quoted", []string{unquote(strconv.Quote(secret))}},
		{"base64", base64Fragments(base64.RawStdEncoding, secret)},
		{"base64url", base64Fragments(base64.RawURLEncoding, secret)},
		{"hex", []string{hex.EncodeToString([]byte(secret))}},
		{"HEX", []string{strings.ToUpper(hex.EncodeToString([]byte(secret)))}},
	}
}

// withLowerEscapes returns s and s with its percent escapes in lower case.
func withLowerEscapes(s string) []string {
	b := []byte(s)
	for i := 0; i+2 < len(b); i++ {
		if b[i] == '%' {
			b[i+1], b[i+2] = lowerHex(b[i+1]), lowerHex(b[i+2])
		}
	}
	return []string{s, string(b)}
}

func lowerHex(c byte) byte {
	if 'A' <= c && c <= 'F' {
		return c + 'a' - 'A'
	}
	return c
}

// jsonBodies returns secret as the inside of a JSON string, with and
// without HTML escaping of <, > and &.
func jsonBodies(secret string) []string {
	var out []string
	for _, html := range []bool{true, false} {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(html)
		_ = enc.Encode(secret) // encoding a string cannot fail
		out = append(out, unquote(strings.TrimSuffix(buf.String(), "\n")))
	}
	return out
}

// unquote strips the surrounding double quotes.
func unquote(s string) string {
	return s[1 : len(s)-1]
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
