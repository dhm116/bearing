package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// errNonFinite is returned for NaN and infinities, which JSON can't hold.
var errNonFinite = errors.New("number is NaN or infinite")

// errInvalidUTF8 is returned for strings that are not valid UTF-8, which
// JCS would otherwise turn into U+FFFD and so collide with other strings.
var errInvalidUTF8 = errors.New("string is not valid UTF-8")

// canonicalJSON returns the RFC 8785 (JCS) form of v, a value as decoded by
// encoding/json into any: nil, bool, float64, string, []any or
// map[string]any.
func canonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeJCS(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// canonicalizeJSON parses JSON text and returns its JCS form.
func canonicalizeJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return canonicalJSON(v)
}

func writeJCS(b *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case float64:
		s, err := jcsNumber(v)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case string:
		if !utf8.ValidString(v) {
			return errInvalidUTF8
		}
		writeJCSString(b, v)
	case []any:
		b.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeJCS(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			if !utf8.ValidString(k) {
				return errInvalidUTF8
			}
			keys = append(keys, k)
		}
		// JCS sorts member names by their UTF-16 code units.
		slices.SortFunc(keys, func(a, c string) int {
			return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(c)))
		})
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJCSString(b, k)
			b.WriteByte(':')
			if err := writeJCS(b, v[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON: unsupported type %T", v)
	}
	return nil
}

// writeJCSString escapes s as ECMAScript's JSON.stringify does.
func writeJCSString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jcsNumber formats f as ECMAScript's Number.prototype.toString does, which
// is what JCS requires.
func jcsNumber(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errNonFinite
	}
	if f == 0 {
		return "0", nil // also -0
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// Shortest round-tripping digits d1.d2...dk and exponent e, so that
	// f = 0.d1...dk × 10^n with n = e+1.
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expText, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, err := strconv.Atoi(expText)
	if err != nil {
		return "", err
	}
	k, n := len(digits), e+1
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		s = digits[:1]
		if k > 1 {
			s += "." + digits[1:]
		}
		exp := n - 1
		if exp >= 0 {
			s += "e+" + strconv.Itoa(exp)
		} else {
			s += "e-" + strconv.Itoa(-exp)
		}
	}
	return sign + s, nil
}

// compact removes insignificant whitespace from JSON text.
func compact(b []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
