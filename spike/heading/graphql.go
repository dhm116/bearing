package heading

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// gqlType is one type of a GraphQL introspection result.
type gqlType struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Fields     []gqlField
	Interfaces []struct {
		Name string `json:"name"`
	} `json:"interfaces"`
}

type gqlField struct {
	Name string `json:"name"`
	Args []struct {
		Name string  `json:"name"`
		Type gqlRef  `json:"type"`
		Def  *string `json:"defaultValue"`
	} `json:"args"`
	Type gqlRef `json:"type"`
}

type gqlRef struct {
	Kind   string  `json:"kind"`
	Name   string  `json:"name"`
	OfType *gqlRef `json:"ofType"`
}

// named unwraps NON_NULL and LIST; list reports whether a LIST was passed.
func (r gqlRef) named() (name string, list bool) {
	for cur := &r; cur != nil; cur = cur.OfType {
		if cur.Kind == "LIST" {
			list = true
		}
		if cur.Name != "" {
			return cur.Name, list
		}
	}
	return "", list
}

// GQLReport is what a GraphQL schema implies for a mapping.
type GQLReport struct {
	Objects, Kinds, Mutations int
	// HumanKeys are kinds with a readable unique field besides the Node id.
	HumanKeys map[string]int
	// Connections are fields returning a Relay connection of a kind.
	Connections int
	// TypedRefs are fields of a kind that return one other kind.
	TypedRefs int
	// FromViewer and FromSeeds count the kinds reachable through fields and
	// connections from Query.viewer alone, and from viewer plus an
	// organization login given in config.
	FromViewer, FromSeeds int
	// RepositoryTypes are the object types named like a repository.
	RepositoryTypes []string
}

// AnalyzeGraphQL reads an introspection result (schema.json).
func AnalyzeGraphQL(path string) (*GQLReport, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the spike reads specs the caller names
	if err != nil {
		return nil, fmt.Errorf("heading: %w", err)
	}
	var doc struct {
		Schema struct {
			Types []gqlType `json:"types"`
		} `json:"__schema"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("heading: %w", err)
	}
	types := map[string]*gqlType{}
	for i := range doc.Schema.Types {
		types[doc.Schema.Types[i].Name] = &doc.Schema.Types[i]
	}
	isKind := func(name string) bool {
		t := types[name]
		if t == nil || t.Kind != "OBJECT" {
			return false
		}
		for _, i := range t.Interfaces {
			if i.Name == "Node" {
				return true
			}
		}
		return false
	}
	// connectionOf returns the kind a Relay connection type lists.
	connectionOf := func(name string) string {
		t := types[name]
		if t == nil || !strings.HasSuffix(name, "Connection") {
			return ""
		}
		for _, f := range t.Fields {
			if f.Name == "nodes" {
				if n, _ := f.Type.named(); isKind(n) {
					return n
				}
			}
		}
		return ""
	}
	r := &GQLReport{HumanKeys: map[string]int{}}
	r.Mutations = len(types["Mutation"].Fields)
	for _, t := range doc.Schema.Types {
		if t.Kind != "OBJECT" {
			continue
		}
		r.Objects++
		if strings.Contains(t.Name, "Repository") && !strings.HasSuffix(t.Name, "Connection") && !strings.HasSuffix(t.Name, "Edge") && !strings.HasSuffix(t.Name, "Payload") {
			r.RepositoryTypes = append(r.RepositoryTypes, t.Name)
		}
		if !isKind(t.Name) {
			continue
		}
		r.Kinds++
		human := "none"
		for _, f := range t.Fields {
			switch f.Name {
			case "nameWithOwner", "login", "slug", "resourcePath":
				if human == "none" || f.Name != "resourcePath" {
					human = f.Name
				}
			}
			n, list := f.Type.named()
			switch {
			case connectionOf(n) != "":
				r.Connections++
			case !list && isKind(n):
				r.TypedRefs++
			}
		}
		r.HumanKeys[human]++
	}
	// Reach: walk object fields and connections from the roots, taking a
	// field only when all its arguments are optional (a seed's login aside).
	reach := func(roots []string) int {
		seen := map[string]bool{}
		queue := append([]string(nil), roots...)
		for len(queue) > 0 {
			name := queue[0]
			queue = queue[1:]
			if seen[name] {
				continue
			}
			seen[name] = true
			for _, f := range types[name].Fields {
				if requiredArgs(f) {
					continue
				}
				n, _ := f.Type.named()
				if c := connectionOf(n); c != "" {
					n = c
				}
				if isKind(n) && !seen[n] {
					queue = append(queue, n)
				}
			}
		}
		return len(seen)
	}
	r.FromViewer = reach([]string{"User"})
	r.FromSeeds = reach([]string{"User", "Organization"})
	sort.Strings(r.RepositoryTypes)
	return r, nil
}

func requiredArgs(f gqlField) bool {
	for _, a := range f.Args {
		if a.Type.Kind == "NON_NULL" && a.Def == nil {
			return true
		}
	}
	return false
}

func (r *GQLReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "object types: %d; kinds (implement Node, so keyed by a global id): %d; mutations excluded: %d\n", r.Objects, r.Kinds, r.Mutations)
	fmt.Fprintf(&b, "readable key besides the id: %s\n", sortedCounts(r.HumanKeys))
	fmt.Fprintf(&b, "Relay connections of kinds: %d (one paging style)\n", r.Connections)
	fmt.Fprintf(&b, "typed single references between kinds: %d\n", r.TypedRefs)
	fmt.Fprintf(&b, "kinds reachable with no config from viewer: %d; with an organization login: %d of %d\n", r.FromViewer, r.FromSeeds, r.Kinds)
	fmt.Fprintf(&b, "object types named like a repository (not connections, edges or payloads): %d\n", len(r.RepositoryTypes))
	return b.String()
}
