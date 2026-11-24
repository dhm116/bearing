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
	cardGap, pad   = 40.0, 40.0 // a corridor between cards holds three runs 12px apart
	marginX        = 72.0       // room for loops left of the first column and right of the last
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
	// corridors are the ys, per column, where an edge can cross it: between
	// and below the cards (above them sits the column label).
	corridors [][]float64
}

func colX(c int) float64 { return marginX + float64(c)*(colW+colGap) }

// gapSpan is the free x range of gap g, the one left of column g.
func (l *layout) gapSpan(g int) (left, right float64) {
	if g == 0 {
		return colX(0) - marginX, colX(0)
	}
	if g == len(l.columns) {
		return colX(g-1) + colW, colX(g-1) + colW + marginX
	}
	return colX(g-1) + colW, colX(g)
}

func layOut(v *view) *layout {
	l := &layout{cards: map[string]box{}, rows: map[string]box{}, col: map[string]int{}, cluster: map[string]string{}}
	ys := []float64{}
	for _, e := range v.Entities {
		if len(l.columns) == 0 || l.columns[len(l.columns)-1] != e.Kind {
			l.columns = append(l.columns, e.Kind)
			ys = append(ys, pad+titleH+24)
			l.corridors = append(l.corridors, nil)
		}
		c := len(l.columns) - 1
		x := colX(c)
		h := cardHead + float64(len(e.Records))*rowH + 6
		l.cards[e.ID] = box{x, ys[c], colW, h}
		l.col[e.ID] = c
		for i, r := range e.Records {
			l.rows[r.ID] = box{x, ys[c] + cardHead + float64(i)*rowH, colW, rowH}
			l.col[r.ID] = c
			l.cluster[r.ID] = e.ID
		}
		ys[c] += h + cardGap
		l.corridors[c] = append(l.corridors[c], ys[c]-cardGap/2)
		l.h = math.Max(l.h, ys[c])
	}
	l.w = marginX*2 + float64(len(l.columns))*colW + float64(max(len(l.columns)-1, 0))*colGap
	l.h += pad
	return l
}

//go:embed graph.css
var graphCSS string

const arrowDefs = `<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 1L9 5L0 9" class="arrowhead"/></marker></defs>`

// neighbourhood is one entity with the entities its facts and identity links
// reach, and only those facts and links: a view small enough to read.
func neighbourhood(v *view, id string) *view {
	keep := map[string]bool{id: true}
	n := &view{Entities: []vEntity{}, Facts: []vFact{}, Links: []vLink{}}
	for _, f := range v.Facts {
		if f.Subject == id || f.Object == id {
			keep[f.Subject], keep[f.Object] = true, true
			n.Facts = append(n.Facts, f)
		}
	}
	owner := map[string]string{}
	for _, e := range v.Entities {
		for _, r := range e.Records {
			owner[r.ID] = e.ID
		}
	}
	for _, k := range v.Links {
		if owner[k.A] == id || owner[k.B] == id {
			keep[owner[k.A]], keep[owner[k.B]] = true, true
			n.Links = append(n.Links, k)
		}
	}
	for _, e := range v.Entities { // already in column order
		if keep[e.ID] {
			n.Entities = append(n.Entities, e)
		}
	}
	return n
}

// svgDrawing draws the view as SVG. Every text is escaped here; ids ride in
// data- attributes so the HTML page can find what was clicked.
func svgDrawing(v *view, title string, embedded bool) string {
	l := layOut(v)
	var b strings.Builder
	e := html.EscapeString
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="%s">`, l.w, l.h, l.w, l.h, e(title))
	b.WriteString(`<desc>Entities by kind, the facts between them and the identity links between their records.</desc>`)
	if !embedded { // the HTML page carries the style and the arrow once for every view
		b.WriteString(`<style>` + graphCSS + `</style>` + arrowDefs)
	}
	fmt.Fprintf(&b, `<rect class="paper" width="%.0f" height="%.0f"/>`, l.w, l.h)
	fmt.Fprintf(&b, `<text class="title" x="%.0f" y="%.0f">%s</text>`, pad, pad+16, e(clip(title, 60)))
	fmt.Fprintf(&b, `<text class="eyebrow" x="%.0f" y="%.0f">%d ENTITIES · %d FACTS · %d IDENTITY LINKS</text>`, pad, pad+38, len(v.Entities), len(v.Facts), len(v.Links))
	for i, k := range l.columns {
		if k == "" {
			k = "named by a fact, reported by no source"
		}
		fmt.Fprintf(&b, `<text class="eyebrow" x="%.1f" y="%.1f">%s</text>`, colX(i), pad+titleH+10, e(strings.ToUpper(k)))
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
	// Routes for facts (card to card, out of the right side) and identity
	// links (record to record, out of the left side), planned together
	// because they share the gaps between columns.
	var rs []route
	for _, p := range order {
		a, z := l.cards[p.s], l.cards[p.o]
		ca, cz := l.col[p.s], l.col[p.o]
		rs = append(rs, newRoute(side{"c:" + p.s, a, ca, ca <= cz}, side{"c:" + p.o, z, cz, ca >= cz}))
	}
	var links, inside []vLink
	var inner []string // drawn after the cards
	for _, k := range v.Links {
		ra, oka := l.rows[k.A]
		rb, okb := l.rows[k.B]
		if !oka || !okb {
			continue
		}
		if l.cluster[k.A] == l.cluster[k.B] { // records of one card: a bracket inside it
			inside = append(inside, k)
			continue
		}
		links = append(links, k)
		ca, cb := l.col[k.A], l.col[k.B]
		rs = append(rs, newRoute(side{"r:" + k.A, ra, ca, ca < cb}, side{"r:" + k.B, rb, cb, ca > cb}))
	}
	routeAll(l, rs)
	hops := crossings(rs)

	b.WriteString(`<g class="facts">`)
	labelled := map[string]bool{} // one label per predicates per target: edges into a card share it
	for i, p := range order {
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
		d := roundedPath(rs[i].points(), hops[i])
		fmt.Fprintf(&b, `<g class="%s" data-facts="%s" data-from="%s" data-to="%s" data-predicates="%s">`, cls, e(strings.Join(ids, " ")), e(p.s), e(p.o), e(strings.Join(preds, " ")))
		fmt.Fprintf(&b, `<path class="hit" d="%s"/><path class="line" d="%s" marker-end="url(#arrow)"/>`, d, d)
		if k := p.o + " " + strings.Join(preds, ", "); !labelled[k] {
			labelled[k] = true
			b.WriteString(edgeLabel(rs[i], strings.ToUpper(strings.Join(preds, ", "))))
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</g>`)

	b.WriteString(`<g class="links">`)
	for i, k := range links {
		cls := "link " + strings.ReplaceAll(k.Kind, "_", "-") + " " + k.Status
		d := roundedPath(rs[len(order)+i].points(), hops[len(order)+i])
		fmt.Fprintf(&b, `<g class="%s" data-link="%s" data-from="%s" data-to="%s"><path class="hit" d="%s"/><path class="line" d="%s"/><title>%s</title></g>`,
			e(cls), e(k.ID), e(l.cluster[k.A]), e(l.cluster[k.B]), d, d, e(linkTip(k)))
	}
	b.WriteString(`</g>`)

	// Links inside one card join its rows with a bracket along its left
	// edge, drawn over the card; brackets that overlap step inward.
	depth := map[string][]float64{} // per card: lowest free y per step
	for _, k := range inside {
		ra, rb := l.rows[k.A], l.rows[k.B]
		y1, y2 := min(ra.y, rb.y)+rowH/2, max(ra.y, rb.y)+rowH/2
		c := l.cluster[k.A]
		step := slices.IndexFunc(depth[c], func(free float64) bool { return free < y1 })
		if step < 0 {
			step, depth[c] = len(depth[c]), append(depth[c], 0)
		}
		depth[c][step] = y2 + 2
		x := ra.x + 4 + float64(min(step, 2))*2
		d := fmt.Sprintf("M%.1f %.1f H%.1f V%.1f H%.1f", x+3, y1, x, y2, x+3)
		cls := "link " + strings.ReplaceAll(k.Kind, "_", "-") + " " + k.Status
		inner = append(inner, fmt.Sprintf(`<g class="%s" data-link="%s" data-from="%s" data-to="%s"><path class="hit" d="%s"/><path class="line" d="%s"/><title>%s</title></g>`,
			e(cls), e(k.ID), e(c), e(c), d, d, e(linkTip(k))))
	}

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
	b.WriteString(`</g><g class="links inside">` + strings.Join(inner, "") + `</g></svg>`)
	return b.String()
}

// edgeLabel draws a label on an opaque mask that stays 7px clear of its
// line: above a horizontal run, or right of a vertical one.
func edgeLabel(r route, text string) string {
	const charW, h, gap = 5.9, 13.0, 7.0
	w := float64(len([]rune(text)))*charW + 8
	x, y, beside := r.label(w)
	rx, ry := x-w/2, y-gap-h
	if beside {
		rx, ry = x+gap, y-h/2
	}
	return fmt.Sprintf(`<rect class="label-mask" x="%.1f" y="%.1f" width="%.1f" height="%.0f" rx="2"/><text class="edge-label" x="%.1f" y="%.1f">%s</text>`,
		rx, ry, w, h, rx+w/2, ry+h-3.5, html.EscapeString(text))
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func writeSVG(w io.Writer, v *view) error {
	_, err := io.WriteString(w, svgDrawing(v, "Bearing graph", false)+"\n")
	return err
}

//go:embed graph.html
var graphPage string

var graphTemplate = template.Must(template.New("graph").Parse(graphPage))

type focusView struct {
	ID  string
	SVG template.HTML
}

// writeHTML writes the whole graph as the overview, and one neighbourhood
// view per entity that a click opens.
// shortcut: every view is drawn up front, so the page grows with entities
// times neighbours; draw views on demand once a server route exists.
func writeHTML(w io.Writer, v *view) error {
	//nolint:gosec // G203: svgDrawing escapes all text; graphCSS is our embedded file
	page := struct {
		CSS   template.CSS
		Defs  template.HTML
		SVG   template.HTML
		Views []focusView
		Data  *view
	}{template.CSS(graphCSS), template.HTML(arrowDefs), template.HTML(svgDrawing(v, "Bearing graph", true)), nil, v}
	for _, e := range v.Entities {
		//nolint:gosec // G203: as above
		page.Views = append(page.Views, focusView{e.ID, template.HTML(svgDrawing(neighbourhood(v, e.ID), e.Name, true))})
	}
	return graphTemplate.Execute(w, page)
}
