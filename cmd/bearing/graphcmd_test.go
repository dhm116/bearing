package main

import (
	"encoding/json"
	"errors"
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
