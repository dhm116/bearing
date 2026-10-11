package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// writeJSON writes v as indented JSON and a newline, with the characters a
// terminal would act on escaped (see jsonSafe).
func writeJSON(w io.Writer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(jsonSafe(b.Bytes()))
	return err
}

// maxIndentedBytes bounds what indenting a body from elsewhere may produce.
const maxIndentedBytes = 64 << 20

// writeJSONBody writes a JSON object that came from elsewhere, such as a
// server's answer, as it is except for indentation and the escaping of jsonSafe,
// so fields this program has no type for stay. A body that would indent to more
// than maxIndentedBytes (deeply nested and wide: every element of a deep array
// is indented by its depth) is written compact instead, so a hostile server
// cannot turn its capped answer into an unbounded one.
func writeJSONBody(w io.Writer, body []byte) error {
	body = bytes.TrimSpace(body)
	if !json.Valid(body) || len(body) == 0 || body[0] != '{' {
		return errors.New("the answer is not a JSON object")
	}
	body = jsonSafe(body)
	var b bytes.Buffer
	var err error
	if indentedSize(body) > maxIndentedBytes {
		err = json.Compact(&b, body)
	} else {
		err = json.Indent(&b, body, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("format the answer: %w", err)
	}
	b.WriteByte('\n')
	_, err = w.Write(b.Bytes())
	return err
}

// indentedSize is an upper bound of the size of body, a valid JSON document,
// once json.Indent has put every element on a line of its own.
func indentedSize(body []byte) int64 {
	n := int64(len(body))
	var depth int64
	inString, escaped := false, false
	for _, c := range body {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[', '{':
			depth++
			n += 1 + 2*depth
		case ']', '}':
			n += 1 + 2*depth
			depth--
		case ',':
			n += 1 + 2*depth
		case ':':
			n++ // the space Indent puts after it
		}
	}
	return n
}

// jsonSafe escapes, as \uXXXX, what a terminal would act on in text that came
// from a source system or a server. encoding/json escapes the C0 controls and
// the line and paragraph separators but passes DEL, the C1 controls
// (U+0080 to U+009F, which include the 8-bit CSI and OSC that some terminals
// honour) and format characters such as bidi overrides. A JSON document holds
// such a character only inside a string, where an escape reads back as the same
// character, so the document means what it meant. Bytes that are not UTF-8 are
// escaped as U+FFFD, which is what a JSON decoder would make of them. Characters
// below U+007F are left alone: they are structure or already escaped.
func jsonSafe(b []byte) []byte {
	var out []byte
	last := 0
	for i := 0; i < len(b); {
		c := b[i]
		if c < utf8.RuneSelf && c != 0x7f {
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 || terminalSensitive(r) {
			out = append(out, b[last:i]...)
			out = appendEscape(out, r)
			last = i + size
		}
		i += size
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// terminalSensitive reports whether r is a control, format, or line or
// paragraph separator character (a superset of what terminalSafe drops that
// JSON's own escaping leaves).
func terminalSensitive(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

func appendEscape(out []byte, r rune) []byte {
	if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError || r2 != utf8.RuneError {
		return fmt.Appendf(out, `\u%04x\u%04x`, r1, r2)
	}
	return fmt.Appendf(out, `\u%04x`, r)
}
