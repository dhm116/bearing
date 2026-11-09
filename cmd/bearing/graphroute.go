package main

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Edges are drawn the way the diagram-design skill draws them: straight
// runs that turn at right angles with rounded corners. Every vertical run
// gets its own lane in the gap between columns, so parallel edges never sit
// on top of each other; an edge that skips a column crosses it through the
// space between two cards.

const (
	corner    = 8.0  // corner radius
	laneInset = 14.0 // no lane closer than this to a card
	laneGap   = 6.0  // vertical runs closer than this share no lane
)

// side is one side of a card or record row, where an edge starts or ends.
type side struct {
	key   string // "c:"+entity ID or "r:"+record ID
	b     box
	col   int
	right bool
}

func (s side) x() float64 {
	if s.right {
		return s.b.x + s.b.w
	}
	return s.b.x
}

type route struct {
	from, to side
	ys       []float64 // y at the start, at each column crossed, at the end
	gaps     []int     // gap of the vertical run between ys[i] and ys[i+1]
	lanes    []float64 // x of each vertical run
}

// newRoute plans the gaps an edge runs through. Gap g is left of column g.
func newRoute(from, to side) route {
	r := route{from: from, to: to}
	switch {
	case from.col == to.col && from.right:
		r.gaps = []int{from.col + 1}
	case from.col == to.col:
		r.gaps = []int{from.col}
	case from.col < to.col:
		for g := from.col + 1; g <= to.col; g++ {
			r.gaps = append(r.gaps, g)
		}
	default:
		for g := from.col; g > to.col; g-- {
			r.gaps = append(r.gaps, g)
		}
	}
	r.ys = make([]float64, len(r.gaps)+1)
	r.lanes = make([]float64, len(r.gaps))
	return r
}

// routeAll fixes where each route meets its boxes, which corridor it takes
// through each column it crosses, and the lane of each vertical run.
func routeAll(l *layout, rs []route) {
	// Ports: the routes meeting one side spread along it, ordered by where
	// their other end is, so they leave without crossing.
	type port struct {
		i     int
		start bool
		other float64
	}
	ports := map[string][]port{}
	var keys []string
	add := func(s side, p port) {
		k := s.key + fmt.Sprint(s.right)
		if ports[k] == nil {
			keys = append(keys, k)
		}
		ports[k] = append(ports[k], p)
	}
	for i, r := range rs {
		add(r.from, port{i, true, r.to.b.y + r.to.b.h/2})
		add(r.to, port{i, false, r.from.b.y + r.from.b.h/2})
	}
	for _, k := range keys {
		ps := ports[k]
		slices.SortStableFunc(ps, func(a, b port) int { return cmp.Compare(a.other, b.other) })
		for n, p := range ps {
			r := &rs[p.i]
			b := r.to.b
			if p.start {
				b = r.from.b
			}
			y := b.y + b.h*float64(n+1)/float64(len(ps)+1)
			if p.start {
				r.ys[0] = y
			} else {
				r.ys[len(r.ys)-1] = y
			}
		}
	}

	// Corridors: a column crossed is crossed between two cards, nearest the
	// middle of the edge; edges sharing a corridor step a few pixels apart.
	used := map[[2]int]int{}
	for i := range rs {
		r := &rs[i]
		mid := (r.ys[0] + r.ys[len(r.ys)-1]) / 2
		step := 1
		if r.to.col < r.from.col {
			step = -1
		}
		for k := 1; k < len(r.ys)-1; k++ {
			c := r.from.col + k*step
			best := 0
			for j, y := range l.corridors[c] {
				if math.Abs(y-mid) < math.Abs(l.corridors[c][best]-mid) {
					best = j
				}
			}
			n := used[[2]int{c, best}]
			used[[2]int{c, best}]++
			r.ys[k] = l.corridors[c][best] + float64([]int{0, 4, -4, 8, -8}[n%5])
		}
	}

	// Lanes: per gap, vertical runs whose spans overlap take different lanes.
	type run struct {
		i, k   int
		lo, hi float64
	}
	runs := map[int][]run{}
	for i, r := range rs {
		for k, g := range r.gaps {
			lo, hi := r.ys[k], r.ys[k+1]
			runs[g] = append(runs[g], run{i, k, min(lo, hi), max(lo, hi)})
		}
	}
	for g, rr := range runs {
		slices.SortFunc(rr, func(a, b run) int { return cmp.Compare(a.lo, b.lo) })
		var ends []float64 // lowest free y per lane
		lane := make([]int, len(rr))
		for j, u := range rr {
			lane[j] = slices.IndexFunc(ends, func(e float64) bool { return e < u.lo })
			if lane[j] < 0 {
				lane[j] = len(ends)
				ends = append(ends, 0)
			}
			ends[lane[j]] = u.hi + laneGap
		}
		left, right := l.gapSpan(g)
		left, right = left+laneInset, right-laneInset
		for j, u := range rr {
			rs[u.i].lanes[u.k] = left + (right-left)*float64(lane[j]+1)/float64(len(ends)+1)
		}
	}
}

// points is the route as a polyline: out of its box, along each lane, in.
func (r route) points() [][2]float64 {
	pts := [][2]float64{{r.from.x(), r.ys[0]}}
	for k, x := range r.lanes {
		pts = append(pts, [2]float64{x, r.ys[k]}, [2]float64{x, r.ys[k+1]})
	}
	return append(pts, [2]float64{r.to.x(), r.ys[len(r.ys)-1]})
}

// label is where an edge's label goes: over its longest horizontal run, or
// beside its vertical run when every horizontal one is short (a loop).
func (r route) label() (x, y float64, anchor string) {
	pts := r.points()
	best := -1.0
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		if a[1] == b[1] && math.Abs(b[0]-a[0]) > best {
			best, x, y = math.Abs(b[0]-a[0]), (a[0]+b[0])/2, a[1]-4
		}
	}
	if best >= 60 {
		return x, y, "middle"
	}
	return r.lanes[0] + 4, (r.ys[0]+r.ys[len(r.ys)-1])/2 + 3, "start"
}

// roundedPath draws a polyline with its corners rounded.
func roundedPath(in [][2]float64) string {
	var p [][2]float64
	for _, q := range in {
		n := len(p)
		switch {
		case n > 0 && p[n-1] == q:
		case n >= 2 && (p[n-2][0] == p[n-1][0] && p[n-1][0] == q[0] || p[n-2][1] == p[n-1][1] && p[n-1][1] == q[1]):
			p[n-1] = q // collinear: extend the run
		default:
			p = append(p, q)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "M%.1f %.1f", p[0][0], p[0][1])
	for i := 1; i < len(p)-1; i++ {
		a, c, n := p[i-1], p[i], p[i+1]
		r := min(corner, dist(a, c)/2, dist(c, n)/2)
		s, e := towards(c, a, r), towards(c, n, r)
		fmt.Fprintf(&b, " L%.1f %.1f Q%.1f %.1f %.1f %.1f", s[0], s[1], c[0], c[1], e[0], e[1])
	}
	last := p[len(p)-1]
	fmt.Fprintf(&b, " L%.1f %.1f", last[0], last[1])
	return b.String()
}

func dist(a, b [2]float64) float64 { return math.Hypot(b[0]-a[0], b[1]-a[1]) }

func towards(from, to [2]float64, d float64) [2]float64 {
	l := dist(from, to)
	return [2]float64{from[0] + (to[0]-from[0])*d/l, from[1] + (to[1]-from[1])*d/l}
}
