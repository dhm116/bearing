package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"bearing.example/pkg/query"
)

// out collects text so a command writes its answer in one piece.
type out struct{ b strings.Builder }

func (o *out) line(indent int, format string, args ...any) {
	o.b.WriteString(strings.Repeat("  ", indent))
	fmt.Fprintf(&o.b, format, args...)
	o.b.WriteByte('\n')
}

func (o *out) flush(w io.Writer) error {
	_, err := io.WriteString(w, o.b.String())
	return err
}

// stamp renders a time in UTC, in RFC 3339 with the fraction only if any.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// percent renders parts per million as a percentage: 950000 is "95%".
func percent(ppm uint32) string {
	s := strconv.FormatFloat(float64(ppm)/10000, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

// span renders a valid-time interval, or "" if it is unbounded.
func span(from, to *time.Time) string {
	switch {
	case from != nil && to != nil:
		return " valid " + stamp(*from) + " until " + stamp(*to)
	case from != nil:
		return " valid from " + stamp(*from)
	case to != nil:
		return " valid until " + stamp(*to)
	}
	return ""
}

// when says what a Point asks for.
func when(p query.Point) string {
	v, r := "now", "now"
	if !p.Valid.IsZero() {
		v = stamp(p.Valid)
	}
	if !p.Recorded.IsZero() {
		r = stamp(p.Recorded)
	}
	return fmt.Sprintf("as of %s, as known %s", v, r)
}

// supports prints the provenance of a fact.
func (o *out) supports(indent int, sups []query.Support) {
	for _, s := range sups {
		adapter := ""
		if s.Adapter != "" {
			adapter = " adapter " + s.Adapter
		}
		o.line(indent, "%s: %s, observed %s, event %s%s", s.Source, percent(s.ConfidencePPM), stamp(s.ObservedAt), s.EventID, adapter)
		valid := strings.TrimSpace(span(s.ValidFrom, s.ValidTo))
		if valid == "" {
			valid = "valid at all times"
		}
		o.line(indent+1, "%s, recorded %s", valid, stamp(s.RecordedAt))
		if s.EvidenceURL != "" {
			o.line(indent+1, "evidence %s", s.EvidenceURL)
		}
		for _, q := range s.Qualifiers {
			b, err := json.Marshal(q)
			if err != nil {
				b = []byte(err.Error())
			}
			o.line(indent+1, "qualifier %s", b)
		}
	}
}

// fact prints a fact's status line and its supports. The subject is named
// only if showSubject, for lists that mix subjects.
func (o *out) fact(indent int, f query.Fact, showSubject bool) {
	subject := ""
	if showSubject {
		subject = f.Subject.String() + " "
	}
	reason := ""
	if f.StatusReason != "none" {
		reason = " (" + f.StatusReason + ")"
	}
	o.line(indent, "%s%s %s: %s %s%s%s", subject, f.Predicate, f.Object, f.Status, percent(f.ConfidencePPM), reason, span(f.ValidFrom, f.ValidTo))
	o.supports(indent+1, f.Supports)
}

func (o *out) conflicts(indent int, cs []query.Conflict) {
	for _, c := range cs {
		resolution := ""
		if c.Resolution != "unspecified" && c.Resolution != "" {
			resolution = ", decided by " + c.Resolution
		}
		o.line(indent, "%s%s%s", c.Predicate, span(c.ValidFrom, c.ValidTo), resolution)
		for _, p := range c.Positions {
			var objs []string
			for _, ob := range p.Objects {
				objs = append(objs, ob.String())
			}
			authority := ""
			if p.Authoritative {
				authority = " (authoritative)"
			}
			o.line(indent+1, "%s%s: %s", p.SourceSystem, authority, strings.Join(objs, ", "))
		}
	}
}

// unmergeHint is printed under a subject's merges. Splitting aliases off the
// survivor of a chain of merges mints a new subject rather than restoring the
// one that was merged first, so an exact undo goes one merge at a time.
const unmergeHint = "hint: to undo a merge, un-merge the aliases the merged subject had when it merged. " +
	"If the survivor has itself been merged into another subject since, un-merging those aliases from the top subject " +
	"creates a new subject instead; to restore the original exactly, un-merge the later merge first, then this one."

func renderEntity(w io.Writer, e *query.Entity) error {
	var o out
	o.line(0, "%s", e.Subject)
	o.line(0, "%s", when(e.Point))
	o.line(0, "status %s, minted %s by event %s", e.Status, stamp(e.MintedAt), e.MintedBy)
	o.line(0, "")
	o.line(0, "keys")
	for _, k := range e.Keys {
		redirect := ""
		if k.Redirect {
			redirect = " (old name, redirects here)"
		}
		o.line(1, "%s%s%s", k.Alias, redirect, span(k.ValidFrom, k.ValidTo))
	}
	o.line(0, "")
	o.line(0, "facts")
	if len(e.Facts) == 0 {
		o.line(1, "none")
	}
	for _, f := range e.Facts {
		o.fact(1, f, false)
	}
	if len(e.Conflicts) > 0 {
		o.line(0, "")
		o.line(0, "conflicts")
		o.conflicts(1, e.Conflicts)
	}
	if len(e.Merges) > 0 {
		o.line(0, "")
		o.line(0, "merges")
		undoable := false
		for _, m := range e.Merges {
			ended := ""
			if m.UnmergedAt != nil {
				ended = ", un-merged " + stamp(*m.UnmergedAt)
			} else if m.Rule != "placeholder" { // a placeholder merge can't be un-merged
				undoable = true
			}
			o.line(1, "%s into %s by %s at %s, event %s%s", m.Merged, m.Survivor, m.Rule, percent(m.ConfidencePPM), m.EventID, ended)
			if rv := m.Review; rv != nil {
				scores := ""
				if rv.SurvivorScorePPM != 0 || rv.MergedScorePPM != 0 {
					scores = fmt.Sprintf(" (survivor side %s, merged side %s)", percent(rv.SurvivorScorePPM), percent(rv.MergedScorePPM))
				}
				o.line(2, "review %s%s, event %s", rv.Status, scores, rv.EventID)
			}
		}
		if undoable {
			o.line(1, "%s", unmergeHint)
		}
	}
	if len(e.Unmerges) > 0 {
		o.line(0, "")
		o.line(0, "un-merges")
		for _, u := range e.Unmerges {
			how := "back to"
			if u.Split {
				how = "to a new subject,"
			}
			o.line(1, "%s left %s %s %s at %s, event %s", strings.Join(u.Aliases, ", "), u.Subject, how, u.Target, stamp(u.RecordedAt), u.EventID)
		}
	}
	return o.flush(w)
}

func renderOwnership(w io.Writer, ow *query.Ownership) error {
	var o out
	o.line(0, "owners of %s", ow.Subject)
	o.line(0, "%s", when(ow.Point))
	o.line(0, "")
	if len(ow.Owners) == 0 {
		o.line(0, "no asserted owner")
	}
	for _, f := range ow.Owners {
		o.line(0, "%s", f.Object)
		o.line(1, "%s %s%s", f.Status, percent(f.ConfidencePPM), span(f.ValidFrom, f.ValidTo))
		o.supports(1, f.Supports)
	}
	if len(ow.NotAsserted) > 0 {
		o.line(0, "")
		o.line(0, "not asserted, so not owners")
		for _, f := range ow.NotAsserted {
			o.fact(1, f, false)
		}
	}
	if len(ow.Conflicts) > 0 {
		o.line(0, "")
		o.line(0, "conflicts")
		o.conflicts(1, ow.Conflicts)
	}
	return o.flush(w)
}

func renderRelations(w io.Writer, r *query.Relations) error {
	var o out
	o.line(0, "relations of %s", r.Subject)
	o.line(0, "%s", when(r.Point))
	o.line(0, "")
	o.line(0, "from this subject")
	if len(r.Out) == 0 {
		o.line(1, "none")
	}
	for _, f := range r.Out {
		o.fact(1, f, false)
	}
	o.line(0, "")
	o.line(0, "to this subject")
	if len(r.In) == 0 {
		o.line(1, "none")
	}
	for _, f := range r.In {
		o.fact(1, f, true)
	}
	return o.flush(w)
}

func renderChanges(w io.Writer, c *query.Changes, continued bool) error {
	var o out
	scope := ""
	if c.Subject != nil {
		scope = " of " + c.Subject.String()
	}
	o.line(0, "changes%s from %s to %s, %s axis", scope, stamp(c.Since), stamp(c.Until), c.Axis)
	if len(c.Changes) == 0 {
		o.line(0, "")
		o.line(0, "%s", nothingChanged(c, continued))
	}
	for _, ch := range c.Changes {
		o.line(0, "")
		o.line(0, "%s %s %s", ch.Subject, ch.Predicate, ch.Object)
		o.line(1, "%s %s to %s %s, changed %s", ch.From.Status, percent(ch.From.ConfidencePPM), ch.To.Status, percent(ch.To.ConfidencePPM), stamp(ch.ChangedAt))
		if len(ch.Before) > 0 {
			o.line(1, "before")
			o.supports(2, ch.Before)
		}
		if len(ch.After) > 0 {
			o.line(1, "after")
			o.supports(2, ch.After)
		}
	}
	if c.NextPageToken != "" {
		o.line(0, "")
		o.line(0, "%d changes shown, newest first. For the next page, run bearing changes --page-token %s with the same --store", len(c.Changes), c.NextPageToken)
	}
	return o.flush(w)
}

// nothingChanged says that a window held no changes and when the newest
// change before it was, if there was one. A page reached by a token that
// comes back empty says only that the changes ran out.
func nothingChanged(c *query.Changes, continued bool) string {
	if continued {
		// The question was answered on an earlier page; the rest of it ran out.
		return "no further changes in this window"
	}
	within := "from " + stamp(c.Since) + " to " + stamp(c.Until)
	then := " before then"
	if c.DefaultWindow {
		within, then = fmt.Sprintf("within the last %d hours", int(c.Until.Sub(c.Since).Hours())), ""
	}
	if c.MostRecentChange == nil {
		return "nothing changed " + within + "; no change has been recorded" + then
	}
	return "nothing changed " + within + "; the most recent change" + then + " occurred on " + stamp(*c.MostRecentChange)
}
