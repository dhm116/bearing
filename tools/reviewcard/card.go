package main

import (
	"fmt"
	"html"
	"strings"
)

// Colors are the site's tokens (site/static/css/tokens.css). The states follow
// the "Today" trail on the home page: .hop--found, --partial, --dead and --wait.
type theme struct {
	name, ink, muted, line, surface, sunk        string
	good, goodSoft, warn, warnSoft, bad, badSoft string
}

var themes = []theme{
	{
		"light", "#18242A", "#55646B", "#D3DCDA", "#FFFFFF", "#EDF1F0",
		"#2F6B45", "#E3F0E7", "#A5561A", "#F7EBDF", "#A33A3A", "#F6E3E3",
	},
	{
		"dark", "#E4ECEA", "#98A8AD", "#2A383D", "#172226", "#131C1F",
		"#8DCB9F", "#1C3325", "#E3A56E", "#3A2819", "#E89A9A", "#3B1F1F",
	},
}

// Icon paths from site/templates/_icons.html (20×20 grid, stroked).
var icons = map[string]string{
	"lock":     `<rect x="4" y="9" width="12" height="8.5" rx="1.5"/><path d="M6.5 9V6.5a3.5 3.5 0 0 1 7 0V9"/>`,
	"layers":   `<path d="m10 3 7 3.5-7 3.5-7-3.5L10 3Z"/><path d="m3 10 7 3.5 7-3.5M3 13.5 10 17l7-3.5"/>`,
	"code":     `<path d="m7 6-4 4 4 4M13 6l4 4-4 4M11 4 9 16"/>`,
	"book":     `<path d="M4 4.5A1.5 1.5 0 0 1 5.5 3H16v12H5.5A1.5 1.5 0 0 0 4 16.5v-12ZM4 16.5A1.5 1.5 0 0 0 5.5 18H16v-3"/>`,
	"eye":      `<path d="M2 10s3-5.5 8-5.5S18 10 18 10s-3 5.5-8 5.5S2 10 2 10Z"/><circle cx="10" cy="10" r="2.25"/>`,
	"check":    `<path d="m4.5 10.5 3.5 3.5 7.5-8"/>`,
	"x":        `<path d="m5.5 5.5 9 9M14.5 5.5l-9 9"/>`,
	"question": `<path d="M7.5 7.5a2.5 2.5 0 1 1 3.5 2.3c-.7.3-1 .9-1 1.6v.6"/><path d="M10 15.5v.01"/>`,
	"wait":     `<circle cx="10" cy="10" r="7"/><path d="M10 6v4l2.5 2"/>`,
}

// personas maps each reviewer in .claude/agents/<name>-reviewer.md to its
// card title and icon.
var personas = map[string][2]string{
	"security": {"Security", "lock"},
	"data":     {"Data", "layers"},
	"go":       {"Go", "code"},
	"spec":     {"Spec", "book"},
	"search":   {"Search", "eye"},
}

const (
	width    = 620 // two cards a row; GitHub scales the image down on narrow screens
	headH    = 30
	gap      = 12
	maxLines = 3 // blocking lines a card shows; the last says "+N more" when there are more
	style    = `<style>.d{font-family:"Bricolage Grotesque","Segoe UI",system-ui,sans-serif;font-weight:700}` +
		`.s{font-family:"IBM Plex Sans","Segoe UI",system-ui,sans-serif}` +
		`.m{font-family:"IBM Plex Mono",ui-monospace,Menlo,monospace;font-weight:500}</style>`
)

// look returns a state's top band, pill text and pill background colors, the
// pill icon and the reviewer's label.
func look(t theme, state string) (band, fg, bg, ic, label string) {
	switch state {
	case "approve":
		return t.good, t.good, t.goodSoft, "check", "Approve"
	case "discuss":
		return t.warn, t.warn, t.warnSoft, "question", "Needs discussion"
	case "changes":
		return t.bad, t.bad, t.badSoft, "x", "Request changes"
	case "running":
		return t.line, t.muted, t.sunk, "wait", "In progress"
	}
	return t.line, t.muted, t.sunk, "wait", "Queued"
}

var headline = map[string]string{
	"running": "In progress", "approve": "Approved", "discuss": "Needs discussion", "changes": "Changes requested",
}

// overall is the round's state: running until every reviewer is done, then
// the worst verdict.
func overall(rs []reviewer) string {
	worst := "approve"
	for _, r := range rs {
		switch r.State {
		case "queued", "running":
			return "running"
		case "changes":
			worst = "changes"
		case "discuss":
			if worst != "changes" {
				worst = "discuss"
			}
		}
	}
	return worst
}

func (r reviewer) done() bool { return r.State != "queued" && r.State != "running" }

func esc(s string) string { return html.EscapeString(s) }

// clip shortens s to n characters with an ellipsis.
// shortcut: counts characters, not rendered width; measure the text if lines overflow.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func icon(name string, x, y, size float64, color string, sw float64) string {
	return fmt.Sprintf(`<g transform="translate(%g %g) scale(%g)" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round" stroke-linejoin="round">%s</g>`,
		x, y, size/20, color, sw, icons[name])
}

// pill draws a verdict chip whose right edge is at right. Its width comes from
// a character count, which holds because the label is monospace.
func pill(right, y float64, label, fg, bg, ic string) string {
	w := 6 + 14 + 4 + float64(len(label))*7.2 + 10
	x := right - w
	return fmt.Sprintf(`<rect x="%g" y="%g" width="%g" height="22" rx="11" fill="%s"/>`, x, y, w, bg) +
		icon(ic, x+6, y+4, 14, fg, 2.2) +
		fmt.Sprintf(`<text x="%g" y="%g" class="m" font-size="12" fill="%s">%s</text>`, x+24, y+15.5, fg, esc(label))
}

// lines is what a card lists under its counts: the blocking findings, capped
// at maxLines.
func lines(r reviewer) []string {
	if len(r.Blockers) <= maxLines {
		return r.Blockers
	}
	return append(r.Blockers[:maxLines-1:maxLines-1], fmt.Sprintf("+%d more", len(r.Blockers)-maxLines+1))
}

func cardH(r reviewer) int { return 64 + 18*len(lines(r)) }

func card(t theme, r reviewer, i int, x, y, w, h float64) string {
	band, fg, bg, ic, label := look(t, r.State)
	p := personas[r.Name]
	var b strings.Builder
	fmt.Fprintf(&b, `<g transform="translate(%g %g)"><clipPath id="c%d"><rect width="%g" height="%g" rx="8"/></clipPath>`, x, y, i, w, h)
	fmt.Fprintf(&b, `<rect x=".5" y=".5" width="%g" height="%g" rx="7.5" fill="%s" stroke="%s"/>`, w-1, h-1, t.surface, t.line)
	fmt.Fprintf(&b, `<rect width="%g" height="4" fill="%s" clip-path="url(#c%d)"/>`, w, band, i)
	b.WriteString(icon(p[1], 16, 16, 18, t.muted, 1.8))
	fmt.Fprintf(&b, `<text x="40" y="30" class="d" font-size="17" fill="%s">%s</text>`, t.ink, esc(p[0]))
	b.WriteString(pill(w-14, 14, label, fg, bg, ic))
	n := func(v int) string { return fmt.Sprintf(`<tspan fill="%s" font-weight="600">%d</tspan>`, t.ink, v) }
	var sum string
	switch {
	case r.State == "running":
		sum = "Reading the change"
	case r.State == "queued":
		sum = "Waiting to start"
	case len(r.Blockers) == 0:
		sum = fmt.Sprintf("No blocking findings · %s %s", n(r.Suggestions), plural(r.Suggestions, "suggestion"))
	default:
		sum = fmt.Sprintf("%s blocking · %s %s", n(len(r.Blockers)), n(r.Suggestions), plural(r.Suggestions, "suggestion"))
	}
	fmt.Fprintf(&b, `<text x="16" y="52" class="s" font-size="13" fill="%s">%s</text>`, t.muted, sum)
	chars := int((w - 46) / 6.3)
	for j, l := range lines(r) {
		ly := 72 + 18*float64(j)
		fmt.Fprintf(&b, `<circle cx="20" cy="%g" r="2.5" fill="%s"/>`, ly-4, fg)
		fmt.Fprintf(&b, `<text x="30" y="%g" class="s" font-size="12.5" fill="%s">%s</text>`, ly, t.ink, esc(clip(l, chars)))
	}
	b.WriteString(`</g>`)
	return b.String()
}

// alt is the image's text alternative: everything the cards show, with the
// blocking findings in full.
func alt(rd round) string {
	parts := []string{fmt.Sprintf("Code review round %d: %s.", rd.Round, strings.ToLower(headline[overall(rd.Reviewers)]))}
	for _, r := range rd.Reviewers {
		_, _, _, _, label := look(themes[0], r.State)
		s := fmt.Sprintf("%s: %s", personas[r.Name][0], strings.ToLower(label))
		if r.done() {
			s += fmt.Sprintf(", %d blocking, %d %s", len(r.Blockers), r.Suggestions, plural(r.Suggestions, "suggestion"))
			if len(r.Blockers) > 0 {
				s += " (" + strings.Join(r.Blockers, "; ") + ")"
			}
		}
		parts = append(parts, s+".")
	}
	return strings.Join(parts, " ")
}

// svg draws the round: a header line, then the cards two to a row (one
// full-width card when the round has a single reviewer). Each row is as tall
// as its tallest card.
func svg(t theme, rd round) string {
	rs := rd.Reviewers
	cols := min(2, len(rs))
	w := float64(width-(cols-1)*gap) / float64(cols)
	var cards strings.Builder
	y := float64(headH + 8)
	for i := 0; i < len(rs); i += cols {
		row := rs[i:min(i+cols, len(rs))]
		h := 0
		for _, r := range row {
			h = max(h, cardH(r))
		}
		for j, r := range row {
			cards.WriteString(card(t, r, i+j, float64(j)*(w+gap), y, w, float64(h)))
		}
		y += float64(h + gap)
	}
	done := 0
	for _, r := range rs {
		if r.done() {
			done++
		}
	}
	h := int(y) - gap
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">%s`,
		width, h, width, h, esc(alt(rd)), style)
	fmt.Fprintf(&b, `<text x="2" y="19" class="m" font-size="12" letter-spacing="1.2" fill="%s"><tspan fill="%s">CODE REVIEW</tspan> · ROUND %d · %d OF %d DONE</text>`,
		t.muted, t.ink, rd.Round, done, len(rs))
	if len(rs) > 1 { // a lone card already shows the verdict
		ov := overall(rs)
		_, fg, bg, ic, _ := look(t, ov)
		b.WriteString(pill(width, 2, headline[ov], fg, bg, ic))
	}
	b.WriteString(cards.String())
	b.WriteString(`</svg>`)
	return b.String()
}
