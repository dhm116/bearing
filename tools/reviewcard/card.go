package main

import (
	"fmt"
	"html"
	"strings"
)

// Colors are the site's tokens (site/static/css/tokens.css). The states follow
// the "Today" trail on the home page: .hop--found, --partial, --dead and --wait.
// accent marks work in progress.
type theme struct {
	name, ink, muted, line, surface, sunk, accent string
	good, goodSoft, warn, warnSoft, bad, badSoft  string
}

var themes = []theme{
	{
		"light", "#18242A", "#55646B", "#D3DCDA", "#FFFFFF", "#EDF1F0", "#1D5E74",
		"#2F6B45", "#E3F0E7", "#A5561A", "#F7EBDF", "#A33A3A", "#F6E3E3",
	},
	{
		"dark", "#E4ECEA", "#98A8AD", "#2A383D", "#172226", "#131C1F", "#7FC0D6",
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
	"spark":    `<path d="M10 2.5c.6 3.9 2.6 5.9 6.5 6.5-3.9.6-5.9 2.6-6.5 6.5-.6-3.9-2.6-5.9-6.5-6.5 3.9-.6 5.9-2.6 6.5-6.5ZM16 14.5l.5 1.5 1.5.5-1.5.5-.5 1.5-.5-1.5-1.5-.5 1.5-.5.5-1.5Z"/>`,
}

// personas maps each reviewer in .claude/agents/<name>-reviewer.md, and the
// ponytail reviewer that checks every round's findings after them, to its card
// title and icon.
var personas = map[string][2]string{
	"security": {"Security", "lock"},
	"data":     {"Data", "layers"},
	"go":       {"Go", "code"},
	"spec":     {"Spec", "book"},
	"search":   {"Search", "eye"},
	"ponytail": {"Ponytail", "spark"},
}

const (
	width    = 620 // GitHub scales the image down on narrow screens
	headH    = 30
	findX    = 174        // where a row's blocking findings start, under its summary
	pillR    = width - 12 // every verdict pill's right edge, header and rows alike
	maxLines = 3          // blocking lines a row shows; the last says "+N more" when there are more
	// Work in progress pulses and its clock hand turns; GitHub serves the SVG
	// as an image, where CSS animation still runs. Reduced motion stops both.
	style = `<style>.d{font-family:"Bricolage Grotesque","Segoe UI",system-ui,sans-serif;font-weight:700}` +
		`.s{font-family:"IBM Plex Sans","Segoe UI",system-ui,sans-serif}` +
		`.m{font-family:"IBM Plex Mono",ui-monospace,Menlo,monospace;font-weight:500}` +
		`@keyframes p{50%{opacity:.3}}.p{animation:p 1.6s ease-in-out infinite}` +
		`@keyframes t{to{transform:rotate(360deg)}}.t{animation:t 2.4s linear infinite;transform-box:fill-box;transform-origin:50% 100%}` +
		`@media (prefers-reduced-motion:reduce){.p,.t{animation:none}}</style>`
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
// a character count, which holds because the label is monospace. While a
// reviewer runs, the clock's hand turns.
func pill(right, y float64, label, fg, bg, ic string, running bool) string {
	w := 6 + 14 + 4 + float64(len(label))*7.2 + 10
	x := right - w
	mark := icon(ic, x+6, y+4, 14, fg, 2.2)
	if running {
		mark = fmt.Sprintf(`<g transform="translate(%g %g) scale(.7)" fill="none" stroke="%s" stroke-width="2.2" stroke-linecap="round">`+
			`<circle cx="10" cy="10" r="7"/><path class="t" d="M10 10V5.5"/></g>`, x+6, y+4, fg)
	}
	return fmt.Sprintf(`<rect x="%g" y="%g" width="%g" height="22" rx="11" fill="%s"/>`, x, y, w, bg) + mark +
		fmt.Sprintf(`<text x="%g" y="%g" class="m" font-size="12" fill="%s">%s</text>`, x+24, y+15.5, fg, esc(label))
}

// lines is what a row lists under its counts: the blocking findings, capped
// at maxLines.
func lines(r reviewer) []string {
	if len(r.Blockers) <= maxLines {
		return r.Blockers
	}
	return append(r.Blockers[:maxLines-1:maxLines-1], fmt.Sprintf("+%d more", len(r.Blockers)-maxLines+1))
}

func rowH(r reviewer) float64 {
	if n := len(lines(r)); n > 0 {
		return 48 + 18*float64(n)
	}
	return 44
}

// row draws one reviewer: a state dot, its name, its counts and its verdict,
// with its blocking findings listed under the counts.
func row(t theme, r reviewer, y float64, first bool) string {
	band, fg, bg, ic, label := look(t, r.State)
	p := personas[r.Name]
	var b strings.Builder
	fmt.Fprintf(&b, `<g transform="translate(0 %g)">`, y)
	if !first {
		fmt.Fprintf(&b, `<rect x="1" width="%d" height="1" fill="%s"/>`, width-2, t.line)
	}
	dot := `<circle cx="16" cy="22" r="4" fill="%s"/>`
	if r.State == "running" {
		band, dot = t.accent, `<circle class="p" cx="16" cy="22" r="4" fill="%s"/>`
	}
	fmt.Fprintf(&b, dot, band)
	b.WriteString(icon(p[1], 30, 13, 18, t.muted, 1.8))
	fmt.Fprintf(&b, `<text x="54" y="28" class="d" font-size="16" fill="%s">%s</text>`, t.ink, esc(p[0]))
	n := func(v int) string { return fmt.Sprintf(`<tspan fill="%s" font-weight="600">%d</tspan>`, t.ink, v) }
	var sum string
	switch {
	case r.State == "running":
		sum = "Reading the change"
	case r.State == "queued":
		sum = "Waiting to start"
	case len(r.Blockers) == 0:
		sum = fmt.Sprintf("No blocking · %s %s", n(r.Suggestions), plural(r.Suggestions, "suggestion"))
	default:
		sum = fmt.Sprintf("%s blocking · %s %s", n(len(r.Blockers)), n(r.Suggestions), plural(r.Suggestions, "suggestion"))
	}
	fmt.Fprintf(&b, `<text x="160" y="27" class="s" font-size="13" fill="%s">%s</text>`, t.muted, sum)
	b.WriteString(pill(pillR, 11, label, fg, bg, ic, r.State == "running"))
	chars := (width - findX - 12) * 10 / 63 // about 6.3px a character at 12.5px
	for j, l := range lines(r) {
		ly := 50 + 18*float64(j)
		fmt.Fprintf(&b, `<circle cx="%d" cy="%g" r="2.5" fill="%s"/>`, findX-10, ly-4, fg)
		fmt.Fprintf(&b, `<text x="%d" y="%g" class="s" font-size="12.5" fill="%s">%s</text>`, findX, ly, t.ink, esc(clip(l, chars)))
	}
	b.WriteString(`</g>`)
	return b.String()
}

// track draws one segment per round a PR may have: earlier rounds in their
// verdict's colour, this one pulsing while it runs, later ones outlined.
func track(t theme, x, y float64, rd round) string {
	var b strings.Builder
	ov := overall(rd.Reviewers)
	for i := 1; i <= maxRounds; i++ {
		px := x + float64(i-1)*20
		var fill, cls string
		switch {
		case i < rd.Round:
			fill, _, _, _, _ = look(t, rd.Previous[i-1])
		case i == rd.Round && ov == "running":
			fill, cls = t.accent, ` class="p"`
		case i == rd.Round:
			fill, _, _, _, _ = look(t, ov)
		default:
			fmt.Fprintf(&b, `<rect x="%g" y="%g" width="15" height="5" rx="2.5" fill="none" stroke="%s"/>`, px+.5, y+.5, t.line)
			continue
		}
		fmt.Fprintf(&b, `<rect%s x="%g" y="%g" width="16" height="6" rx="3" fill="%s"/>`, cls, px, y, fill)
	}
	return b.String()
}

// alt is the image's text alternative: everything the picture shows, with the
// blocking findings in full.
func alt(rd round) string {
	parts := []string{fmt.Sprintf("Code review round %d of %d: %s.", rd.Round, maxRounds, strings.ToLower(headline[overall(rd.Reviewers)]))}
	for i, v := range rd.Previous {
		parts = append(parts, fmt.Sprintf("Round %d: %s.", i+1, strings.ToLower(headline[v])))
	}
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

// svg draws the round: a header with the round track and the overall
// verdict, then one row per reviewer in a single panel.
func svg(t theme, rd round) string {
	rs := rd.Reviewers
	var rows strings.Builder
	y := 0.0
	done := 0
	for i, r := range rs {
		rows.WriteString(row(t, r, y, i == 0))
		y += rowH(r)
		if r.done() {
			done++
		}
	}
	top := float64(headH + 8)
	h := int(top + y)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">%s`,
		width, h, width, h, esc(alt(rd)), style)
	head := fmt.Sprintf("ROUND %d OF %d", rd.Round, maxRounds)
	fmt.Fprintf(&b, `<text x="2" y="19" class="m" font-size="12" letter-spacing="1.2" fill="%s"><tspan fill="%s">%s</tspan></text>`, t.muted, t.ink, head)
	tx := 2 + float64(len(head))*8.4 + 10
	b.WriteString(track(t, tx, 10, rd))
	fmt.Fprintf(&b, `<text x="%g" y="19" class="m" font-size="12" letter-spacing="1.2" fill="%s">%d OF %d DONE</text>`, tx+maxRounds*20+8, t.muted, done, len(rs))
	if len(rs) > 1 { // a lone row already shows the verdict
		ov := overall(rs)
		_, fg, bg, ic, _ := look(t, ov)
		b.WriteString(pill(pillR, 2, headline[ov], fg, bg, ic, ov == "running"))
	}
	fmt.Fprintf(&b, `<g transform="translate(0 %g)"><rect x=".5" y=".5" width="%d" height="%g" rx="7.5" fill="%s" stroke="%s"/>%s</g>`,
		top, width-1, y-1, t.surface, t.line, rows.String())
	b.WriteString(`</svg>`)
	return b.String()
}
