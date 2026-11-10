package main

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// writeTLD writes the view as a tld bulk import (https://github.com/Mertcikla/tld),
// so tld draws, routes and lets people drill down: the root view holds one
// element per kind, each kind's view its entities, and an entity with more
// than one record a view of its records joined by their identity links.
// Facts are connectors in the subject's kind view; tld shows the ones whose
// object sits elsewhere as off-view links. JSON is valid YAML, which tld reads.
func writeTLD(w io.Writer, v *view) error {
	type placement struct {
		Parent string `json:"parent"`
	}
	type element struct {
		Name        string      `json:"name"`
		Kind        string      `json:"kind,omitempty"`
		Description string      `json:"description,omitempty"`
		Technology  string      `json:"technology,omitempty"`
		Tags        []string    `json:"tags,omitempty"`
		HasView     bool        `json:"has_view,omitempty"`
		ViewLabel   string      `json:"view_label,omitempty"`
		Placements  []placement `json:"placements"`
	}
	type connector struct {
		View         string `json:"view"`
		Source       string `json:"source"`
		Target       string `json:"target"`
		Label        string `json:"label"`
		Description  string `json:"description,omitempty"`
		Relationship string `json:"relationship,omitempty"`
		Direction    string `json:"direction,omitempty"`
		Style        string `json:"style"`
	}
	doc := struct {
		Elements   map[string]element `json:"elements"`
		Connectors []connector        `json:"connectors"`
	}{map[string]element{}, []connector{}}

	// An entity's ID is its oldest record's ID, so the two need their own maps.
	refOf, recRef := map[string]string{}, map[string]string{}
	kindRef := map[string]string{}
	owner := map[string]string{} // record ID -> entity ID
	for _, e := range v.Entities {
		kind := cmp.Or(e.Kind, "Unseen")
		if kindRef[kind] == "" {
			kindRef[kind] = "kind." + tldSlug(kind)
			doc.Elements[kindRef[kind]] = element{
				Name: kindTitle(e.Kind), Kind: "group", HasView: true, ViewLabel: "Entities",
				Description: kindNote(e.Kind), Placements: []placement{{"root"}},
			}
		}
		ref := tldSlug(kind) + "." + tldSlug(e.Name) + "-" + tldSlug(e.ID[max(0, len(e.ID)-8):]) // UUIDv7s share their leading time bits
		refOf[e.ID] = ref
		var desc []string
		for _, r := range e.Records {
			owner[r.ID] = e.ID
			desc = append(desc, cmp.Or(strings.Join(r.Sources, ", "), "no source")+": "+strings.Join(append(slices.Clone(r.Keys), r.StableIDs...), ", "))
		}
		for _, a := range e.Attributes {
			desc = append(desc, fmt.Sprintf("%s = %v", a.Predicate, a.Value))
		}
		el := element{
			Name: e.Name, Kind: tldKind(e.Kind), Description: strings.Join(desc, "\n"),
			Technology: kind, Tags: []string{tldSlug(kind)}, Placements: []placement{{kindRef[kind]}},
		}
		if len(e.Records) > 1 {
			el.HasView, el.ViewLabel = true, "Records"
			for i, r := range e.Records {
				rref := fmt.Sprintf("%s.r%d", ref, i+1)
				recRef[r.ID] = rref
				doc.Elements[rref] = element{
					Name: cmp.Or(r.Label, r.Name), Kind: "record", Technology: cmp.Or(strings.Join(r.Sources, ", "), "no source"),
					Description: strings.Join(append(slices.Clone(r.Keys), r.StableIDs...), "\n"), Placements: []placement{{ref}},
				}
			}
		}
		doc.Elements[ref] = el
	}

	// One connector per subject, object, predicate and status: tld keys a
	// connector by its ends and label, and the evidence paths behind one
	// fact (GitHub and the directory both saying it) belong on one line.
	idx := map[string]int{}
	for _, f := range v.Facts {
		s, o := refOf[f.Subject], refOf[f.Object]
		label := f.Predicate
		if f.Status != "asserted" {
			label += " (proposed)"
		}
		var by []string
		for _, src := range f.Sources {
			if !slices.Contains(by, src.Source) {
				by = append(by, src.Source)
			}
		}
		desc := "said by " + strings.Join(by, ", ") + " since " + f.ValidFrom.UTC().Format("2006-01-02 15:04")
		for _, k := range slices.Sorted(maps.Keys(f.Qualifiers)) {
			desc += fmt.Sprintf("; %s: %v", k, f.Qualifiers[k])
		}
		key := s + " " + o + " " + label
		if i, ok := idx[key]; ok {
			doc.Connectors[i].Description += "\n" + desc
			continue
		}
		idx[key] = len(doc.Connectors)
		view := doc.Elements[s].Placements[0].Parent
		doc.Connectors = append(doc.Connectors, connector{View: view, Source: s, Target: o, Label: label, Description: f.Status + ": " + desc, Relationship: f.Predicate, Style: "smoothstep"})
	}
	for _, l := range v.Links {
		a, b := recRef[l.A], recRef[l.B]
		if a == "" || b == "" || owner[l.A] != owner[l.B] {
			continue // shortcut: links across entities need a shared view; none in Acme yet
		}
		label := map[string]string{"same_as": "same thing", "distinct_from": "not the same"}[l.Kind]
		if l.Status != "asserted" {
			label = "maybe the same"
		}
		doc.Connectors = append(doc.Connectors, connector{
			View: refOf[owner[l.A]], Source: a, Target: b, Label: label, Relationship: l.Kind, Direction: "none", Style: "smoothstep",
			Description: l.Status + " because of " + l.Rule + " since " + l.ValidFrom.UTC().Format("2006-01-02 15:04"),
		})
	}
	return writeJSON(w, doc)
}

// tldSlug makes an element ref segment: lowercase letters, digits and hyphens.
func tldSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	return cmp.Or(strings.Trim(b.String(), "-"), "x")
}

// tldKind maps a Bearing kind to a tld kind, which picks the icon.
func tldKind(kind string) string {
	switch kind {
	case "Person":
		return "person"
	case "Team":
		return "team"
	case "Repository":
		return "repository"
	case "Component":
		return "service"
	case "Environment":
		return "environment"
	case "":
		return "external"
	}
	return strings.ToLower(kind)
}

func kindTitle(kind string) string {
	if kind == "" {
		return "Named, never reported"
	}
	return kind
}

func kindNote(kind string) string {
	if kind == "" {
		return "Records a fact names but no configured source has reported."
	}
	return "Every " + kind + " Bearing knows."
}
