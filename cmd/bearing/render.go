package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
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

// qualifiers renders a relation's details as " (http, port 8080)": the
// protocol bare and first, then the rest as "name value" in name order.
func qualifiers(q map[string]any) string {
	if len(q) == 0 {
		return ""
	}
	var parts []string
	if p, ok := q["protocol"]; ok {
		parts = append(parts, fmt.Sprint(p))
	}
	for _, k := range slices.Sorted(maps.Keys(q)) {
		if k != "protocol" {
			parts = append(parts, fmt.Sprintf("%s %v", k, q[k]))
		}
	}
	return " (" + strings.Join(parts, ", ") + ")"
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
	o.line(indent, "%s%s %s%s: %s %s%s", subject, f.Predicate, what(f), qualifiers(f.Qualifiers), f.Status, percent(f.Confidence), valid)
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

func renderOwnership(w io.Writer, ow *query.Ownership, at time.Time, ref string) error {
	var o out
	o.subjects("owners of", ref, ow.Answer)
	o.line(0, "%s", when(at))
	o.line(0, "")
	if len(ow.Claims) == 0 {
		o.line(0, "no asserted owner")
	}
	o.answer(ow.Answer, at, false)
	for _, t := range ow.Teams {
		o.line(0, "")
		o.line(0, "people on %s", refText(t.Team))
		if len(t.Members) == 0 {
			o.line(1, "none")
		}
		for _, m := range t.Members {
			role := m.Role
			if role == "" {
				role = "no role given"
			}
			o.line(1, "%s: %s, since %s", refText(m.Ref), role, stamp(m.Since))
			o.sources(2, m.Sources)
		}
		if t.Truncated {
			o.line(1, "more people than shown (%d)", query.MaxTeamMembers)
		}
	}
	return o.flush(w)
}

func renderRelated(w io.Writer, r *query.Related, at time.Time, ref string) error {
	var o out
	o.subjects("relations of", ref, r.Answer)
	o.line(0, "direction %s, %s", r.Direction, when(at))
	o.line(0, "")
	if len(r.Claims) == 0 {
		o.line(0, "none")
	}
	o.answer(r.Answer, at, true)
	return o.flush(w)
}

// subjects names the records an answer starts from: the one a key named, or
// every record a name lookup found, with how it matched.
func (o *out) subjects(title, ref string, a query.Answer) {
	if len(a.Subjects) == 1 && a.Subjects[0].Match == query.MatchKey {
		o.line(0, "%s %s", title, refText(a.Subjects[0].Ref))
		return
	}
	o.line(0, "%s %q: %d record(s) match", title, ref, len(a.Subjects))
	for _, s := range a.Subjects {
		how := "by name"
		if s.Match == query.MatchFuzzy {
			how = "fuzzily, " + percent(s.Score)
		}
		o.line(1, "%s: %s", refText(s.Ref), how)
	}
	if a.LimitHit {
		o.line(1, "more match than shown (%d); give a key to choose one", query.MaxSubjects)
	}
}

// answer prints each claim as a sentence with its evidence, then how the
// claims' ends relate.
func (o *out) answer(a query.Answer, at time.Time, showSubject bool) {
	for _, c := range a.Claims {
		o.line(0, "%s", claimSentence(c, at))
		o.fact(1, c.Fact, showSubject)
		if c.Start.Match == query.MatchFuzzy {
			o.line(1, "starts at %s, which only resembles the question (%s)", refText(c.Start.Ref), percent(c.Start.Score))
		}
		if c.SameAs > 0 || c.Facts > 1 {
			o.line(1, "path: %s", pathText(c.Path))
		}
		for _, ch := range c.ChangedSince {
			if ch.Kind == "closed" {
				o.line(1, "since then: this closed, recorded %s", stamp(ch.At))
			} else {
				o.line(1, "since then: %s, recorded %s", factSentence(ch.Fact), stamp(ch.At))
			}
		}
	}
	if len(a.Relations) > 0 {
		o.line(0, "")
	}
	for _, r := range a.Relations {
		o.line(0, "%s", relationSentence(r, a.Claims))
		for _, l := range r.Links {
			o.line(1, "%s %s %s by %s, strength %s", l.ID, l.Kind, l.Status, l.Rule, percent(l.Score.Strength))
			for _, d := range l.Deposits {
				o.line(2, "%s", depositText(d))
			}
		}
	}
}

// The CLI's sentences, generated from an answer's data here and nowhere
// else (ADR 11). A proposal is worded "likely", never as a fact.

// claimSentence is "github-acme says payments-api (Repository) approves_changes
// Platform (Team).", with "As of" for a past answer.
func claimSentence(c query.Claim, at time.Time) string {
	s := factSentence(c.Fact) + "."
	if !at.IsZero() {
		s = "As of " + stamp(at) + ", " + s
	}
	return s
}

// factSentence is what a fact's sources say, without the full stop.
func factSentence(f query.Fact) string {
	var names []string
	for _, s := range f.Sources {
		if !slices.Contains(names, s.Source) {
			names = append(names, s.Source)
		}
	}
	slices.Sort(names)
	who := strings.Join(names, " and ")
	if who == "" {
		who = "Bearing"
	}
	likely := ""
	if f.Status != "asserted" {
		likely = "likely "
	}
	return fmt.Sprintf("%s says %s %s%s %s", who, refName(f.Subject), likely, f.Predicate, whatName(f))
}

// relationSentence says how two clusters among the claims' objects relate.
func relationSentence(r query.Relation, claims []query.Claim) string {
	a, b := clusterName(r.Clusters[0], claims), clusterName(r.Clusters[1], claims)
	strength := ""
	for _, l := range r.Links {
		if l.Status != "asserted" {
			strength = fmt.Sprintf(" (strength %s%s)", percent(l.Score.Strength), featureText(l))
			break
		}
	}
	switch r.Kind {
	case query.RelationDistinct:
		s := fmt.Sprintf("%s and %s are not the same: an asserted distinct_from says so.", a, b)
		if strength != "" {
			s += " A proposal says otherwise" + strength + "."
		}
		return s
	case query.RelationAssertedNotJoined:
		why := "a distinct_from keeps them apart"
		if r.Reason == query.ReasonKinds {
			why = "their kinds differ"
		}
		return fmt.Sprintf("An asserted same_as names %s and %s as one, but they are not joined: %s.", a, b, why)
	case query.RelationLikelyDistinct:
		return fmt.Sprintf("%s and %s are likely not the same%s.", a, b, strength)
	}
	return fmt.Sprintf("%s and %s are likely the same%s.", a, b, strength)
}

// featureText is the strongest deposit's features, as "; shared_members 0.92".
func featureText(l query.Link) string {
	var best map[string]float64
	var w float64
	for _, d := range l.Deposits {
		if !d.Against && len(d.Features) > 0 && d.Weight >= w {
			best, w = d.Features, d.Weight
		}
	}
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(best)) {
		fmt.Fprintf(&b, "; %s %.2f", k, best[k])
	}
	return b.String()
}

// clusterName names a cluster by the claims' objects in it.
func clusterName(id string, claims []query.Claim) string {
	var names []string
	for _, c := range claims {
		if o := c.Fact.Object; o != nil && o.Cluster == id && !slices.Contains(names, refName(*o)) {
			names = append(names, refName(*o))
		}
	}
	if len(names) == 0 {
		return id
	}
	return strings.Join(names, " / ")
}

// refName is "payments-api (Repository)".
func refName(r query.Ref) string {
	name := cmp.Or(r.Name, r.Key, r.ID)
	if r.Kind != "" {
		name += " (" + r.Kind + ")"
	}
	return name
}

func whatName(f query.Fact) string {
	if f.Object != nil {
		return refName(*f.Object)
	}
	return what(f)
}

// pathText is a claim's path on one line: "a =same_as(l1)= b -owned_by-> c".
func pathText(path []query.PathStep) string {
	if len(path) == 0 {
		return ""
	}
	parts := []string{refName(path[0].From)}
	for _, p := range path {
		to := "(value)"
		if p.To != nil {
			to = refName(*p.To)
		}
		if p.FactID == "" {
			parts = append(parts, "=same_as("+strings.Join(p.Links, ", ")+")=", to)
		} else {
			parts = append(parts, "-"+p.Predicate+"->", to)
		}
	}
	return strings.Join(parts, " ")
}

func renderChanges(w io.Writer, c *query.Changes, ref string) error {
	var o out
	scope := ""
	if ref != "" {
		scope = " of " + ref
	}
	o.line(0, "changes%s from %s to %s", scope, stamp(c.Since), stamp(c.Until))
	if len(c.Changes) == 0 && len(c.Identity) == 0 {
		o.line(0, "")
		o.line(0, "nothing changed")
	}
	for _, ch := range c.Changes {
		o.line(0, "")
		o.line(0, "%s %s", stamp(ch.At), ch.Kind)
		o.fact(1, ch.Fact, true)
	}
	for _, ev := range c.Identity {
		o.line(0, "")
		o.line(0, "%s %s", stamp(ev.At), strings.ReplaceAll(ev.Kind, "_", " "))
		o.line(1, "%s and %s", linkRecordText(ev.A), linkRecordText(ev.B))
		j := ev.Justification
		why := "by " + j.Rule
		if j.ObservationID != "" {
			why += ", observation " + j.ObservationID
		}
		if j.Actor != "" {
			why += ", decided by " + j.Actor
		}
		if j.Reason != "" {
			why += ": " + strconv.Quote(j.Reason)
		}
		if j.Decision != "" {
			why += " (decision " + j.Decision + ")"
		}
		o.line(1, "%s", why)
		for _, l := range ev.Links {
			s := fmt.Sprintf("%s %s %s, %s", l.ID, l.Kind, l.Change, l.Effect)
			if l.PromotedFrom != "" {
				s += ", promoted from " + l.PromotedFrom
			}
			o.line(2, "%s", s)
		}
	}
	return o.flush(w)
}

// renderRuntimes writes one line per entity with its declared versions and
// files; with --below, those below first, then the ones that could not be
// read, then a count of the rest.
func renderRuntimes(w io.Writer, r *query.Runtimes) error {
	var o out
	group := func(title string, es []query.RuntimeEntity) {
		if len(es) == 0 {
			return
		}
		o.line(0, "")
		o.line(0, "%s", title)
		for _, e := range es {
			decl := make([]string, len(e.Declarations))
			for i, d := range e.Declarations {
				decl[i] = d.Version + " (" + d.File + ")"
			}
			o.line(1, "%s: %s", refText(e.Subject), strings.Join(decl, ", "))
		}
	}
	if r.BelowVersion == "" {
		o.line(0, "%s declarations", r.Language)
		if len(r.Entities) == 0 {
			o.line(0, "")
			o.line(0, "none")
		}
		group("declared", r.Entities)
	} else {
		o.line(0, "%s below %s", r.Language, r.BelowVersion)
		group("below", r.Below)
		group("version not readable", r.Unknown)
		o.line(0, "")
		o.line(0, "%d at or above", ptrOr0(r.AtOrAbove))
	}
	if r.Truncated {
		o.line(0, "")
		o.line(0, "more than the limit; the lists are cut")
	}
	return o.flush(w)
}

func ptrOr0(n *int) int {
	if n == nil {
		return 0
	}
	return *n
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

// renderLinks writes the identity links of `bearing links`, strongest first.
func renderLinks(w io.Writer, ls []query.Link, proposed bool) error {
	var o out
	what := "open identity links"
	if proposed {
		what = "proposed identity links"
	}
	if len(ls) == 0 {
		o.line(0, "no %s", what)
		return o.flush(w)
	}
	o.line(0, "%d %s, strongest first", len(ls), what)
	for _, l := range ls {
		o.line(0, "")
		o.line(1, "%s  %s %s by %s  strength %s, certainty %s", l.ID, l.Kind, l.Status, l.Rule, percent(l.Score.Strength), percent(l.Score.Certainty))
		for _, end := range []struct {
			label string
			r     query.LinkRecord
		}{{"a", l.A}, {"b", l.B}} {
			o.line(2, "%s: %s", end.label, linkRecordText(end.r))
		}
		for _, d := range l.Deposits {
			o.line(2, "%s", depositText(d))
		}
	}
	return o.flush(w)
}

// linkRecordText renders a record as `payments [Team id] key (source, source)`.
func linkRecordText(r query.LinkRecord) string {
	s := refText(query.Ref{ID: r.ID, Kind: r.Kind, Name: r.Name, Key: r.Key})
	if len(r.Sources) > 0 {
		s += " (" + strings.Join(r.Sources, ", ") + ")"
	}
	return s
}

// depositText renders one piece of evidence on one line.
func depositText(d query.LinkDeposit) string {
	var b strings.Builder
	side := "for"
	if d.Against {
		side = "against"
	}
	fmt.Fprintf(&b, "evidence %s %s weight %.2f", stamp(d.At), side, d.Weight)
	for _, k := range slices.Sorted(maps.Keys(d.Features)) {
		fmt.Fprintf(&b, " %s=%.2f", k, d.Features[k])
	}
	if d.ObservationID != "" {
		b.WriteString(" observation " + d.ObservationID)
	}
	if d.Actor != "" {
		b.WriteString(" decided by " + d.Actor)
	}
	if d.Reason != "" {
		b.WriteString(": " + strconv.Quote(d.Reason))
	}
	return b.String()
}
