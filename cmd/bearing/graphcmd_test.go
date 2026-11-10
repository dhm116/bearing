package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestGraphJSONHoldsEntitiesFactsAndLinks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	out, err := w.cli("graph", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var v view
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	names := map[string]string{}
	for _, e := range v.Entities {
		names[e.ID] = e.Kind + ":" + e.Name
	}
	var got []string
	for _, f := range v.Facts {
		got = append(got, names[f.Subject]+" "+f.Predicate+" "+names[f.Object])
		if f.Predicate == "depends_on" && f.Qualifiers["protocol"] == "http" && len(f.Sources) == 0 {
			t.Errorf("fact %s has no sources", f.ID)
		}
	}
	for _, want := range []string{
		"Repository:payments-api approves_changes Team:Platform",
		"Person:Jane Doe member_of Team:Payments",
		"Repository:billing owned_by Team:Platform",
	} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("facts lack %q; got:\n%s", want, strings.Join(got, "\n"))
		}
	}
	if strings.Contains(strings.Join(got, "\n"), "approves_changes Team:Payments") {
		t.Errorf("the closed CODEOWNERS fact is drawn; got:\n%s", strings.Join(got, "\n"))
	}
	if v.Entities[0].Kind != "Person" {
		t.Errorf("first column is %q, want Person", v.Entities[0].Kind)
	}
}

func TestGraphEscapesSourceText(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const evil = `<script>alert(1)</script>"x\`
	w.ingest(githubSource, nowAt, entity("Team", "github:team/acme/evil", evil))
	for _, format := range []string{"svg", "html"} {
		t.Run(format, func(t *testing.T) {
			out, err := w.cli("graph", "--format", format)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "<script>alert") {
				t.Errorf("%s output carries the name unescaped", format)
			}
		})
	}
	t.Run("dot", func(t *testing.T) {
		out, err := w.cli("graph", "--format", "dot")
		if err != nil {
			t.Fatal(err)
		}
		// Inside a DOT string only the quote and backslash need escaping.
		if want := `</script>\"x\\`; !strings.Contains(out, want) {
			t.Errorf("dot output lacks the escaped name %s", want)
		}
	})
}

func TestGraphRefusesUnknownFormat(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	if _, err := w.cli("graph", "--format", "png"); !errors.Is(err, errUsage) {
		t.Errorf("got %v, want a usage error", err)
	}
}

func TestGraphEdgesCrossColumnsBetweenCards(t *testing.T) {
	t.Parallel()
	// Four columns, three cards each; edges from the last column back to the
	// first must thread between the cards of the two in between.
	v := &view{}
	for _, kind := range []string{"Person", "Team", "Repository", "Component"} {
		for i := range 3 {
			id := kind + strconv.Itoa(i)
			v.Entities = append(v.Entities, vEntity{ID: id, Kind: kind, Name: id, Records: []vRecord{{ID: "r" + id}, {ID: "s" + id}}})
		}
	}
	for i := range 3 {
		v.Facts = append(v.Facts, vFact{Subject: "Component" + strconv.Itoa(i), Predicate: "owned_by", Object: "Person" + strconv.Itoa(2-i)})
	}
	l := layOut(v)
	var rs []route
	for _, f := range v.Facts {
		ca, co := l.col[f.Subject], l.col[f.Object]
		rs = append(rs, newRoute(side{"c:" + f.Subject, l.cards[f.Subject], ca, false}, side{"c:" + f.Object, l.cards[f.Object], co, true}))
	}
	routeAll(l, rs)
	for i, r := range rs {
		pts := r.points()
		for j := 1; j < len(pts); j++ {
			a, b := pts[j-1], pts[j]
			if a[0] != b[0] && a[1] != b[1] {
				t.Errorf("route %d segment %v-%v is not straight", i, a, b)
			}
			for id, c := range l.cards {
				if id == v.Facts[i].Subject || id == v.Facts[i].Object {
					continue
				}
				lo, hi := min(a[0], b[0]), max(a[0], b[0])
				ylo, yhi := min(a[1], b[1]), max(a[1], b[1])
				if lo < c.x+c.w && hi > c.x && ylo < c.y+c.h && yhi > c.y {
					t.Errorf("route %d segment %v-%v crosses card %s", i, a, b, id)
				}
			}
		}
	}
}

func TestRoundedPathRoundsEachCorner(t *testing.T) {
	t.Parallel()
	got := roundedPath([][2]float64{{0, 0}, {20, 0}, {40, 0}, {40, 30}, {40, 30}, {60, 30}}, nil)
	want := "M0.0 0.0 L32.0 0.0 Q40.0 0.0 40.0 8.0 L40.0 22.0 Q40.0 30.0 48.0 30.0 L60.0 30.0"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRoundedPathHopsOverCrossings(t *testing.T) {
	t.Parallel()
	got := roundedPath([][2]float64{{0, 0}, {100, 0}}, map[float64][]float64{0: {50, 3, 53}})
	// 3 is too near the start and 53 too near the hop at 50: one hop only.
	want := "M0.0 0.0 L45.0 0.0 a5 5 0 0 1 10.0 0 L100.0 0.0"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGraphNeighbourhoodKeepsOnlyWhatTouchesTheEntity(t *testing.T) {
	t.Parallel()
	v := &view{
		Entities: []vEntity{
			{ID: "jane", Kind: "Person", Records: []vRecord{{ID: "j1"}, {ID: "j2"}}},
			{ID: "mei", Kind: "Person", Records: []vRecord{{ID: "m1"}}},
			{ID: "pay", Kind: "Team", Records: []vRecord{{ID: "p1"}}},
			{ID: "plat", Kind: "Team", Records: []vRecord{{ID: "q1"}}},
		},
		Facts: []vFact{
			{ID: "f1", Subject: "jane", Predicate: "member_of", Object: "pay"},
			{ID: "f2", Subject: "mei", Predicate: "member_of", Object: "plat"},
			{ID: "f3", Subject: "pay", Predicate: "member_of", Object: "plat"},
		},
		Links: []vLink{{ID: "l1", A: "j1", B: "j2"}, {ID: "l2", A: "m1", B: "j1", Kind: "distinct_from"}},
	}
	n := neighbourhood(v, "jane")
	var ids []string
	for _, e := range n.Entities {
		ids = append(ids, e.ID)
	}
	if got, want := strings.Join(ids, " "), "jane mei pay"; got != want {
		t.Errorf("entities: got %q, want %q", got, want)
	}
	if len(n.Facts) != 1 || n.Facts[0].ID != "f1" {
		t.Errorf("facts: got %v, want only f1", n.Facts)
	}
	if len(n.Links) != 2 {
		t.Errorf("links: got %d, want 2", len(n.Links))
	}
}
