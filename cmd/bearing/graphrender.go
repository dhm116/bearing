package main

import (
	_ "embed"
	"fmt"
	"html"
	"html/template"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

func writeViewJSON(w io.Writer, v *view) error { return writeJSON(w, v) }

// writeDOT writes Graphviz DOT: one cluster box per entity around its
// records, facts between clusters (compound edges), identity links between
// records. Render it with `dot -Tsvg`.
func writeDOT(w io.Writer, v *view) error {
	var b strings.Builder
	b.WriteString("digraph bearing {\n  compound=true; rankdir=LR; newrank=true;\n")
	b.WriteString("  node [shape=box, style=\"rounded,filled\", fillcolor=\"#F5F7F6\", color=\"#D3DCDA\", fontname=\"IBM Plex Sans\", fontsize=10];\n")
	b.WriteString("  edge [color=\"#55646B\", fontname=\"IBM Plex Mono\", fontsize=9];\n")
	first := map[string]string{} // entity -> a record inside its cluster, for compound edges
	for i, e := range v.Entities {
		fmt.Fprintf(&b, "  subgraph cluster_%d {\n    label=%s; style=\"rounded\"; color=\"#1D5E74\"; fontname=\"IBM Plex Sans\";\n", i, dotQuote(e.Kind+": "+e.Name))
		fmt.Fprintf(&b, "    tooltip=%s;\n", dotQuote(entityTip(e)))
		for _, r := range e.Records {
			if first[e.ID] == "" {
				first[e.ID] = r.ID
			}
			fmt.Fprintf(&b, "    %s [label=%s, tooltip=%s];\n", dotQuote(r.ID), dotQuote(r.Name+"\n"+r.Label), dotQuote(strings.Join(r.Keys, "\n")+"\nsources: "+strings.Join(r.Sources, ", ")))
		}
		b.WriteString("  }\n")
	}
	for _, f := range v.Facts {
		style := "solid"
		if f.Status != "asserted" {
			style = "dashed"
		}
		fmt.Fprintf(&b, "  %s -> %s [label=%s, style=%s, ltail=%s, lhead=%s, tooltip=%s];\n",
			dotQuote(first[f.Subject]), dotQuote(first[f.Object]), dotQuote(f.Predicate), style,
			dotQuote(clusterName(v, f.Subject)), dotQuote(clusterName(v, f.Object)), dotQuote(factTip(f)))
	}
	for _, l := range v.Links {
		color, style := "#4B5198", "dashed"
		switch {
		case l.Kind == "distinct_from":
			color, style = "#A33A3A", "dotted"
		case l.Status == "asserted":
			style = "solid"
		}
		fmt.Fprintf(&b, "  %s -> %s [dir=none, constraint=false, color=%s, style=%s, label=%s, tooltip=%s];\n",
			dotQuote(l.A), dotQuote(l.B), dotQuote(color), style, dotQuote(l.Kind), dotQuote(linkTip(l)))
	}
	b.WriteString("}\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func clusterName(v *view, id string) string {
	for i, e := range v.Entities {
		if e.ID == id {
			return "cluster_" + strconv.Itoa(i)
		}
	}
	return ""
}

// dotQuote quotes s as a DOT string; DOT strings escape only quotes and
// backslashes, and \n is a line break in a label.
func dotQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func entityTip(e vEntity) string {
	lines := []string{e.Kind + ": " + e.Name}
	for _, a := range e.Attributes {
		lines = append(lines, fmt.Sprintf("%s = %v", a.Predicate, a.Value))
	}
	return strings.Join(lines, "\n")
}

func factTip(f vFact) string {
	lines := []string{f.Predicate + " (" + f.Status + ")"}
	for k, q := range f.Qualifiers {
		lines = append(lines, fmt.Sprintf("%s: %v", k, q))
	}
	for _, s := range f.Sources {
		lines = append(lines, s.Source+" "+stamp(s.ObservedAt))
	}
	slices.Sort(lines[1:])
	return strings.Join(lines, "\n")
}

func linkTip(l vLink) string {
	return fmt.Sprintf("%s %s by %s, %d deposits", l.Status, l.Kind, l.Rule, len(l.Deposits))
}

// Layout: one column per kind, entities as cards stacked by name, records as
// rows inside. Facts leave a card's right side; identity links leave a
// record's left side.
const (
	colW, colGap   = 230.0, 150.0
	cardHead, rowH = 38.0, 30.0
	cardGap, pad   = 16.0, 40.0
	titleH         = 56.0
)

type box struct{ x, y, w, h float64 }

type layout struct {
	w, h    float64
	cards   map[string]box // entity ID
	rows    map[string]box // record ID
	col     map[string]int // entity or record ID -> column
	cluster map[string]string
	columns []string // kind per column
}

func layOut(v *view) *layout {
	l := &layout{cards: map[string]box{}, rows: map[string]box{}, col: map[string]int{}, cluster: map[string]string{}}
	ys := []float64{}
	for _, e := range v.Entities {
		if len(l.columns) == 0 || l.columns[len(l.columns)-1] != e.Kind {
			l.columns = append(l.columns, e.Kind)
			ys = append(ys, pad+titleH+24)
		}
		c := len(l.columns) - 1
		x := pad + float64(c)*(colW+colGap)
		h := cardHead + float64(len(e.Records))*rowH + 6
		l.cards[e.ID] = box{x, ys[c], colW, h}
		l.col[e.ID] = c
		for i, r := range e.Records {
			l.rows[r.ID] = box{x, ys[c] + cardHead + float64(i)*rowH, colW, rowH}
			l.col[r.ID] = c
			l.cluster[r.ID] = e.ID
		}
		ys[c] += h + cardGap
		l.h = math.Max(l.h, ys[c])
	}
	l.w = pad*2 + float64(len(l.columns))*colW + float64(max(len(l.columns)-1, 0))*colGap
	l.h += pad
	return l
}

// edgePath joins two boxes: side to facing side across columns, or a loop out
// of one side (right for facts, left for links) within a column.
func edgePath(a, b box, ca, cb int, left bool) (d string, mx, my float64) {
	ay, by := a.y+a.h/2, b.y+b.h/2
	if a.y == b.y && ca == cb { // shortcut: a self-loop is drawn as a small hook
		ay, by = a.y+8, a.y+a.h-8
	}
	switch {
	case ca == cb:
		x := a.x + a.w
		bulge := 30 + math.Abs(by-ay)*0.12
		if left {
			x, bulge = a.x, -bulge
		}
		return fmt.Sprintf("M%.1f %.1f C%.1f %.1f %.1f %.1f %.1f %.1f", x, ay, x+bulge, ay, x+bulge, by, x, by), x + bulge*0.75, (ay + by) / 2
	case ca < cb:
		x1, x2 := a.x+a.w, b.x
		m := (x1 + x2) / 2
		return fmt.Sprintf("M%.1f %.1f C%.1f %.1f %.1f %.1f %.1f %.1f", x1, ay, m, ay, m, by, x2, by), m, (ay + by) / 2
	default:
		x1, x2 := a.x, b.x+b.w
		m := (x1 + x2) / 2
		return fmt.Sprintf("M%.1f %.1f C%.1f %.1f %.1f %.1f %.1f %.1f", x1, ay, m, ay, m, by, x2, by), m, (ay + by) / 2
	}
}

//go:embed graph.css
var graphCSS string

// svgDrawing draws the view as SVG. Every text is escaped here; ids ride in
// data- attributes so the HTML page can find what was clicked.
func svgDrawing(v *view) string {
	l := layOut(v)
	var b strings.Builder
	e := html.EscapeString
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="Bearing graph">`, l.w, l.h, l.w, l.h)
	b.WriteString(`<desc>Entities by kind, the facts between them and the identity links between their records.</desc>`)
	b.WriteString(`<style>` + graphCSS + `</style>`)
	b.WriteString(`<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 1L9 5L0 9" class="arrowhead"/></marker></defs>`)
	fmt.Fprintf(&b, `<rect class="paper" width="%.0f" height="%.0f"/>`, l.w, l.h)
	fmt.Fprintf(&b, `<text class="title" x="%.0f" y="%.0f">Bearing graph</text>`, pad, pad+16)
	fmt.Fprintf(&b, `<text class="eyebrow" x="%.0f" y="%.0f">%d ENTITIES · %d FACTS · %d IDENTITY LINKS</text>`, pad, pad+38, len(v.Entities), len(v.Facts), len(v.Links))
	for i, k := range l.columns {
		if k == "" {
			k = "named by a fact, reported by no source"
		}
		fmt.Fprintf(&b, `<text class="eyebrow" x="%.1f" y="%.1f">%s</text>`, pad+float64(i)*(colW+colGap), pad+titleH+10, e(strings.ToUpper(k)))
	}

	// Facts, merged per pair of entities so parallel predicates share a curve.
	type pair struct{ s, o string }
	merged := map[pair][]vFact{}
	var order []pair
	for _, f := range v.Facts {
		p := pair{f.Subject, f.Object}
		if merged[p] == nil {
			order = append(order, p)
		}
		merged[p] = append(merged[p], f)
	}
	b.WriteString(`<g class="facts">`)
	for _, p := range order {
		fs := merged[p]
		var ids, preds []string
		cls := "fact proposed"
		for _, f := range fs {
			ids = append(ids, f.ID)
			if !slices.Contains(preds, f.Predicate) {
				preds = append(preds, f.Predicate)
			}
			if f.Status == "asserted" {
				cls = "fact"
			}
		}
		d, mx, my := edgePath(l.cards[p.s], l.cards[p.o], l.col[p.s], l.col[p.o], false)
		fmt.Fprintf(&b, `<g class="%s" data-facts="%s" data-from="%s" data-to="%s" data-predicates="%s">`, cls, e(strings.Join(ids, " ")), e(p.s), e(p.o), e(strings.Join(preds, " ")))
		fmt.Fprintf(&b, `<path class="hit" d="%s"/><path class="line" d="%s" marker-end="url(#arrow)"/>`, d, d)
		fmt.Fprintf(&b, `<text class="edge-label" x="%.1f" y="%.1f">%s</text></g>`, mx, my-4, e(strings.Join(preds, ", ")))
	}
	b.WriteString(`</g>`)

	b.WriteString(`<g class="links">`)
	for _, k := range v.Links {
		ra, oka := l.rows[k.A]
		rb, okb := l.rows[k.B]
		if !oka || !okb {
			continue
		}
		cls := "link " + strings.ReplaceAll(k.Kind, "_", "-") + " " + k.Status
		d, _, _ := edgePath(ra, rb, l.col[k.A], l.col[k.B], true)
		fmt.Fprintf(&b, `<g class="%s" data-link="%s" data-from="%s" data-to="%s"><path class="hit" d="%s"/><path class="line" d="%s"/><title>%s</title></g>`,
			e(cls), e(k.ID), e(l.cluster[k.A]), e(l.cluster[k.B]), d, d, e(linkTip(k)))
	}
	b.WriteString(`</g>`)

	b.WriteString(`<g class="cards">`)
	for _, en := range v.Entities {
		c := l.cards[en.ID]
		cls := "card"
		if en.Kind == "" {
			cls += " unseen"
		}
		fmt.Fprintf(&b, `<g class="%s" data-entity="%s" tabindex="0">`, cls, e(en.ID))
		fmt.Fprintf(&b, `<rect class="card-box" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="6"/>`, c.x, c.y, c.w, c.h)
		fmt.Fprintf(&b, `<text class="name" x="%.1f" y="%.1f">%s</text>`, c.x+12, c.y+24, e(clip(en.Name, 28)))
		if len(en.Records) > 1 {
			fmt.Fprintf(&b, `<text class="count" x="%.1f" y="%.1f">%d records</text>`, c.x+c.w-12, c.y+24, len(en.Records))
		}
		for _, r := range en.Records {
			rw := l.rows[r.ID]
			fmt.Fprintf(&b, `<g class="record" data-record="%s"><line class="rule" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`, e(r.ID), rw.x+12, rw.y, rw.x+rw.w-12, rw.y)
			fmt.Fprintf(&b, `<text class="key" x="%.1f" y="%.1f">%s</text>`, rw.x+12, rw.y+19, e(clip(r.Label, 34)))
			b.WriteString(`<title>` + e(strings.Join(r.Keys, "\n")+"\nsources: "+strings.Join(r.Sources, ", ")) + `</title></g>`)
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</g></svg>`)
	return b.String()
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func writeSVG(w io.Writer, v *view) error {
	_, err := io.WriteString(w, svgDrawing(v)+"\n")
	return err
}

//go:embed graph.html
var graphPage string

var graphTemplate = template.Must(template.New("graph").Parse(graphPage))

func writeHTML(w io.Writer, v *view) error {
	return graphTemplate.Execute(w, struct {
		SVG  template.HTML // built by svgDrawing, which escapes every text
		Data *view
	}{template.HTML(svgDrawing(v)), v}) //nolint:gosec // G203: svgDrawing escapes all text
}
