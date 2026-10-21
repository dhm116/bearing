package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"bearing.example/pkg/query"
)

// out collects text so a command writes its answer in one piece.
type out struct{ b strings.Builder }

func (o *out) line(indent int, format string, args ...any) {
	o.b.WriteString(strings.Repeat("  ", indent))
	o.b.WriteString(terminalSafe(fmt.Sprintf(format, args...)))
	o.b.WriteByte('\n')
}

// terminalSafe drops what a terminal would act on, so text that came from a
// source system, or from a server, cannot move the cursor, retitle the window
// or reorder what the operator reads. --json escapes the same set (jsonSafe).
func terminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, s)
}

func (o *out) flush(w io.Writer) error {
	_, err := io.WriteString(w, o.b.String())
	return err
}

// stamp renders a time in UTC, in RFC 3339 with the fraction only if any.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// percent renders a confidence between 0 and 1 as a percentage: 0.95 is
// "95%".
func percent(c float64) string {
	s := strconv.FormatFloat(c*100, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

// refText renders a Ref as `Payments [Team 01a0…] github:team/acme/payments`.
func refText(r query.Ref) string {
	var b strings.Builder
	if r.Name != "" {
		b.WriteString(r.Name + " ")
	}
	if r.Kind != "" {
		fmt.Fprintf(&b, "[%s %s]", r.Kind, r.ID)
	} else {
		fmt.Fprintf(&b, "[%s]", r.ID)
	}
	if r.Key != "" {
		b.WriteString(" " + r.Key)
	}
	return b.String()
}

// when says what time a question was asked at.
func when(at time.Time) string {
	if at.IsZero() {
		return "as of now"
	}
	return "as of " + stamp(at)
}

// what renders a fact's object or value.
func what(f query.Fact) string {
	if f.Object != nil {
		return refText(*f.Object)
	}
	b, err := json.Marshal(f.Value)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// fact prints a fact and its sources. The subject is named only if
// showSubject, for lists that mix subjects.
func (o *out) fact(indent int, f query.Fact, showSubject bool) {
	subject := ""
	if showSubject {
		subject = refText(f.Subject) + " "
	}
	valid := " valid from " + stamp(f.ValidFrom)
	if f.ValidTo != nil {
		valid += " until " + stamp(*f.ValidTo)
	}
	o.line(indent, "%s%s %s: %s %s%s", subject, f.Predicate, what(f), f.Status, percent(f.Confidence), valid)
	o.sources(indent+1, f.Sources)
}

// sources prints the provenance of a fact.
func (o *out) sources(indent int, srcs []query.Source) {
	for _, s := range srcs {
		o.line(indent, "%s: observed %s, observation %s", s.Source, stamp(s.ObservedAt), s.ObservationID)
		if s.URL != "" {
			o.line(indent+1, "evidence %s", s.URL)
		}
	}
}

func renderEntity(w io.Writer, e *query.Entity, at time.Time) error {
	var o out
	o.line(0, "%s", refText(e.Ref))
	o.line(0, "%s", when(at))
	o.line(0, "")
	o.line(0, "keys")
	for _, k := range e.Keys {
		o.line(1, "%s", k)
	}
	o.line(0, "")
	o.line(0, "facts")
	if len(e.Facts) == 0 {
		o.line(1, "none")
	}
	for _, f := range e.Facts {
		o.fact(1, f, false)
	}
	return o.flush(w)
}

func renderOwnership(w io.Writer, ow *query.Ownership, at time.Time) error {
	var o out
	o.line(0, "owners of %s", refText(ow.Subject))
	o.line(0, "%s", when(at))
	o.line(0, "")
	if len(ow.Owners) == 0 {
		o.line(0, "no asserted owner")
	}
	for _, f := range ow.Owners {
		o.fact(0, f, false)
	}
	return o.flush(w)
}

func renderRelations(w io.Writer, r *query.Relations, at time.Time) error {
	var o out
	o.line(0, "relations of %s, direction %s", refText(r.Subject), r.Direction)
	o.line(0, "%s", when(at))
	o.line(0, "")
	if len(r.Steps) == 0 {
		o.line(0, "none")
	}
	for _, s := range r.Steps {
		o.line(0, "depth %d", s.Depth)
		o.fact(1, s.Fact, true)
	}
	return o.flush(w)
}

func renderChanges(w io.Writer, c *query.Changes, ref string) error {
	var o out
	scope := ""
	if ref != "" {
		scope = " of " + ref
	}
	o.line(0, "changes%s from %s to %s", scope, stamp(c.Since), stamp(c.Until))
	if len(c.Changes) == 0 {
		o.line(0, "")
		o.line(0, "nothing changed")
	}
	for _, ch := range c.Changes {
		o.line(0, "")
		o.line(0, "%s %s", stamp(ch.At), ch.Kind)
		o.fact(1, ch.Fact, true)
	}
	return o.flush(w)
}

func renderMatches(w io.Writer, ms []query.Match) error {
	var o out
	if len(ms) == 0 {
		o.line(0, "no match")
	}
	for _, m := range ms {
		o.line(0, "%s %s", percent(m.Score), refText(m.Ref))
	}
	return o.flush(w)
}
