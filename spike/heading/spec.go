// Package heading is a spike for ADR 12: it reads an OpenAPI 3 document,
// guesses what a Heading mapping would say, and runs a mapping against an
// API to see what a generated adapter would emit. Spike code: never merged.
package heading

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Spec is an OpenAPI 3 document read as plain JSON. Only local $refs
// (#/components/...) are followed; the specs the spike reads use no others.
type Spec struct {
	raw map[string]any
}

// Load reads an OpenAPI 3 JSON document.
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the spike reads specs the caller names
	if err != nil {
		return nil, fmt.Errorf("heading: load: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("heading: load %s: %w", path, err)
	}
	return &Spec{raw: raw}, nil
}

// Merge joins documents that describe one API in parts, as a Kubernetes
// cluster serves one per group-version: paths and components are united,
// and the first document's info is kept.
func Merge(specs ...*Spec) *Spec {
	paths, comps := map[string]any{}, map[string]any{}
	for _, s := range specs {
		for k, v := range obj(s.raw["paths"]) {
			paths[k] = v
		}
		for section, entries := range obj(s.raw["components"]) {
			dst := obj(comps[section])
			if dst == nil {
				dst = map[string]any{}
				comps[section] = dst
			}
			for k, v := range obj(entries) {
				dst[k] = v
			}
		}
	}
	return &Spec{raw: map[string]any{"info": specs[0].raw["info"], "paths": paths, "components": comps}}
}

// Title is info.title.
func (s *Spec) Title() string { return str(obj(s.raw["info"])["title"]) }

// deref follows a chain of local $refs and returns the last name (the
// component's key) and the object it names. An allOf with a single member
// is treated as that member.
func (s *Spec) deref(v any) (string, map[string]any) {
	m := obj(v)
	name := ""
	for range 16 {
		if ref := str(m["$ref"]); ref != "" {
			name = ref[strings.LastIndex(ref, "/")+1:]
			m = s.pointer(ref)
			continue
		}
		if all := arr(m["allOf"]); len(all) == 1 {
			m = obj(all[0])
			continue
		}
		break
	}
	return name, m
}

func (s *Spec) pointer(ref string) map[string]any {
	cur := any(s.raw)
	for _, p := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		p = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
		cur = obj(cur)[p]
	}
	return obj(cur)
}

// Param is an operation parameter.
type Param struct{ Name, In string }

// Operation is one method on one path.
type Operation struct {
	Method, Path, ID string
	Params           []Param
	// Response is the 200 JSON response schema, unresolved; nil if none.
	Response any
	// Headers are the 200 response's header names, lowercased.
	Headers []string
	Raw     map[string]any
}

// PathParams are the operation's path parameter names in path order.
func (o Operation) PathParams() []string {
	var out []string
	for _, seg := range strings.Split(o.Path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, seg[1:len(seg)-1])
		}
	}
	return out
}

// QueryParam reports whether the operation takes the query parameter name.
func (o Operation) QueryParam(name string) bool {
	for _, p := range o.Params {
		if p.In == "query" && p.Name == name {
			return true
		}
	}
	return false
}

var methods = []string{"get", "query", "head", "options", "post", "put", "patch", "delete"}

// Operations lists every operation, sorted by path then method.
func (s *Spec) Operations() []Operation {
	paths := obj(s.raw["paths"])
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Operation
	for _, path := range keys {
		item := obj(paths[path])
		shared := s.params(item["parameters"])
		for _, m := range methods {
			raw, ok := item[m].(map[string]any)
			if !ok {
				continue
			}
			op := Operation{Method: strings.ToUpper(m), Path: path, ID: str(raw["operationId"]), Raw: raw}
			op.Params = append(append(op.Params, shared...), s.params(raw["parameters"])...)
			_, resp := s.deref(obj(raw["responses"])["200"])
			op.Response = jsonSchema(obj(resp["content"]))
			for h := range obj(resp["headers"]) {
				op.Headers = append(op.Headers, strings.ToLower(h))
			}
			out = append(out, op)
		}
	}
	return out
}

func (s *Spec) params(v any) []Param {
	var out []Param
	for _, p := range arr(v) {
		_, m := s.deref(p)
		out = append(out, Param{Name: str(m["name"]), In: str(m["in"])})
	}
	return out
}

// jsonSchema picks the schema of the JSON media type, if any.
func jsonSchema(content map[string]any) any {
	if c, ok := content["application/json"]; ok {
		return obj(c)["schema"]
	}
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.Contains(k, "json") {
			return obj(content[k])["schema"]
		}
	}
	return nil
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
