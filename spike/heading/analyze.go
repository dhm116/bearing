package heading

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Kind is a response schema that would become a configured kind.
type Kind struct {
	Schema, Name string
	Lists, Items []Operation
	// Key is the guessed key fields; KeyFrom says how ("route", "id" or "").
	Key     []string
	KeyFrom string
	Refs    []Ref
}

// Ref is a field of a kind that names another kind.
type Ref struct {
	Field, Target string
	// Asserted is true for a typed reference (a $ref to a kind), false for a
	// guess from the field's name.
	Asserted bool
}

// Report is what the analyzer could and could not guess about a spec.
type Report struct {
	Title, Namespace string
	Methods          map[string]int
	SearchPosts      []string
	ListOps, ItemOps int
	// OtherOps are GETs whose response is not a named object or a list of one.
	OtherOps   int
	Kinds      []*Kind
	Collisions int
	// ReachableLists are list operations whose path parameters all come from
	// another reachable list; the rest need config.
	ReachableLists int
	Unresolved     map[string]int
	Paging         map[string]int
	Sensitive      int
}

// Analyze guesses the parts of a Heading mapping that a spec implies.
func Analyze(s *Spec) *Report {
	r := &Report{Title: s.Title(), Namespace: namespaceOf(s.Title()), Methods: map[string]int{}, Unresolved: map[string]int{}, Paging: map[string]int{}}
	kinds := map[string]*Kind{}
	kind := func(schema string) *Kind {
		if kinds[schema] == nil {
			kinds[schema] = &Kind{Schema: schema}
		}
		return kinds[schema]
	}
	var lists []Operation
	for _, op := range s.Operations() {
		r.Methods[op.Method]++
		if op.Method == "POST" && searchy.MatchString(strings.ToLower(op.ID+" "+op.Path)) {
			r.SearchPosts = append(r.SearchPosts, op.Method+" "+op.Path)
		}
		if op.Method != "GET" && op.Method != "QUERY" {
			continue
		}
		switch shape, schema := classify(s, op); shape {
		case "list":
			r.ListOps++
			k := kind(schema)
			k.Lists = append(k.Lists, op)
			lists = append(lists, op)
		case "item":
			r.ItemOps++
			k := kind(schema)
			k.Items = append(k.Items, op)
		default:
			r.OtherOps++
		}
	}
	for _, k := range kinds {
		r.Kinds = append(r.Kinds, k)
	}
	sort.Slice(r.Kinds, func(i, j int) bool { return r.Kinds[i].Schema < r.Kinds[j].Schema })
	r.Collisions = nameKinds(r.Kinds)
	for _, k := range r.Kinds {
		_, sch := s.deref(componentRef(k.Schema))
		k.Key, k.KeyFrom = guessKey(s, sch, k.Items)
		k.Refs = refs(s, sch, kinds, k.Schema, "", 0)
		r.Sensitive += sensitive(s, sch, 0)
	}
	r.ReachableLists = reach(r, lists, kinds, s)
	for _, op := range lists {
		r.Paging[paging(op)]++
	}
	return r
}

var searchy = regexp.MustCompile(`search|query|list|lookup|batch|bulk`)

func componentRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + strings.ReplaceAll(name, "/", "~1")}
}

// classify says whether a GET returns a list of a named schema, one named
// schema, or something else, and which schema.
func classify(s *Spec, op Operation) (string, string) {
	if op.Response == nil {
		return "other", ""
	}
	name, sch := s.deref(op.Response)
	if str(sch["type"]) == "array" {
		if n, _ := s.deref(sch["items"]); n != "" {
			return "list", n
		}
		return "other", ""
	}
	props := obj(sch["properties"])
	if it := obj(props["items"]); str(it["type"]) == "array" {
		if n, _ := s.deref(it["items"]); n != "" {
			return "list", n
		}
	}
	var wrapped []string
	for _, p := range props {
		if pm := obj(p); str(pm["type"]) == "array" {
			if n, m := s.deref(pm["items"]); n != "" && m["properties"] != nil {
				wrapped = append(wrapped, n)
			}
		}
	}
	if len(wrapped) == 1 && name == "" {
		return "list", wrapped[0]
	}
	if name != "" && sch["properties"] != nil {
		return "item", name
	}
	return "other", ""
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// nameKinds sets each kind's name from the last segment of its schema name,
// using two segments where one collides, and returns the collisions.
func nameKinds(ks []*Kind) int {
	short := func(schema string, segs int) string {
		parts := strings.Split(schema, ".")
		if segs > len(parts) {
			segs = len(parts)
		}
		var b strings.Builder
		for _, w := range nonAlnum.Split(strings.Join(parts[len(parts)-segs:], "-"), -1) {
			if w != "" {
				b.WriteString(strings.ToUpper(w[:1]) + w[1:])
			}
		}
		return b.String()
	}
	count := map[string]int{}
	for _, k := range ks {
		count[short(k.Schema, 1)]++
	}
	collisions := 0
	for _, k := range ks {
		k.Name = short(k.Schema, 1)
		if count[k.Name] > 1 {
			collisions++
			k.Name = short(k.Schema, 2)
		}
	}
	return collisions
}

// guessKey takes the shortest item route and maps each path parameter to a
// scalar field of the same name; failing that, an id field.
func guessKey(s *Spec, sch map[string]any, items []Operation) ([]string, string) {
	sort.Slice(items, func(i, j int) bool { return len(items[i].Path) < len(items[j].Path) })
	if len(items) > 0 {
		var key []string
		for _, p := range items[0].PathParams() {
			f := findField(s, sch, p, "", 0)
			if f == "" {
				key = nil
				break
			}
			key = append(key, f)
		}
		if key != nil {
			return key, "route"
		}
	}
	if _, ok := obj(sch["properties"])["id"]; ok {
		return []string{"id"}, "id"
	}
	return nil, ""
}

func findField(s *Spec, sch map[string]any, leaf, prefix string, depth int) string {
	props := obj(sch["properties"])
	if _, p := s.deref(props[leaf]); p != nil && p["properties"] == nil {
		return prefix + leaf
	}
	if depth >= 1 {
		return ""
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, sub := s.deref(props[n]); sub["properties"] != nil {
			if f := findField(s, sub, leaf, prefix+n+".", depth+1); f != "" {
				return f
			}
		}
	}
	return ""
}

var idField = regexp.MustCompile(`^[a-z][a-z0-9_]*_id$|^[a-z][A-Za-z0-9]*Id$`)

// refs walks a kind's schema three levels deep for fields that name a kind.
func refs(s *Spec, sch map[string]any, kinds map[string]*Kind, self, prefix string, depth int) []Ref {
	if depth > 2 {
		return nil
	}
	var out []Ref
	props := obj(sch["properties"])
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := obj(props[n])
		if str(p["type"]) == "array" {
			p = obj(p["items"])
		}
		name, m := s.deref(p)
		switch {
		case name != "" && name != self && kinds[name] != nil:
			out = append(out, Ref{Field: prefix + n, Target: name, Asserted: true})
		case name != "" || m["properties"] != nil:
			out = append(out, refs(s, m, kinds, self, prefix+n+".", depth+1)...)
		case idField.MatchString(n) && n != "id":
			out = append(out, Ref{Field: prefix + n})
		}
	}
	return out
}

func sensitive(s *Spec, sch map[string]any, depth int) int {
	if depth > 2 {
		return 0
	}
	n := 0
	for _, p := range obj(sch["properties"]) {
		_, m := s.deref(p)
		if m["writeOnly"] == true || str(m["format"]) == "password" {
			n++
		}
		if m["properties"] != nil {
			n += sensitive(s, m, depth+1)
		}
	}
	return n
}

// reach finds the list operations whose path parameters can all be filled
// from another reachable list: a parameter p is filled by a listable kind
// whose name starts with p (namespace by Namespace, org by Organization).
func reach(r *Report, lists []Operation, kinds map[string]*Kind, s *Spec) int {
	byName := map[string]bool{}
	reached := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for i, op := range lists {
			if reached[i] {
				continue
			}
			ok := true
			for _, p := range op.PathParams() {
				if !provides(byName, p) {
					ok = false
				}
			}
			if ok {
				reached[i] = true
				_, schema := classify(s, op)
				byName[strings.ToLower(kinds[schema].Name)] = true
				changed = true
			}
		}
	}
	for i, op := range lists {
		if reached[i] {
			continue
		}
		for _, p := range op.PathParams() {
			if !provides(byName, p) {
				r.Unresolved[p]++
			}
		}
	}
	return len(reached)
}

func provides(names map[string]bool, param string) bool {
	p := strings.ToLower(nonAlnum.ReplaceAllString(param, ""))
	for n := range names {
		if strings.HasPrefix(n, p) || strings.HasSuffix(n, p) {
			return true
		}
	}
	return false
}

func paging(op Operation) string {
	style := "none"
	switch {
	case op.QueryParam("continue"):
		style = "cursor (continue)"
	case op.QueryParam("cursor"), op.QueryParam("after"), op.QueryParam("before"):
		style = "cursor"
	case op.QueryParam("page"):
		style = "page"
	case op.QueryParam("offset"):
		style = "offset"
	}
	for _, h := range op.Headers {
		if h == "link" {
			style += " + Link header"
		}
	}
	return style
}

func namespaceOf(title string) string {
	w := strings.Fields(strings.ToLower(title))
	if len(w) == 0 {
		return "api"
	}
	return nonAlnum.ReplaceAllString(w[0], "")
}

// String renders the report's counts.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "spec %q -> namespace %s\n", r.Title, r.Namespace)
	fmt.Fprintf(&b, "operations by method: %s\n", sortedCounts(r.Methods))
	fmt.Fprintf(&b, "POSTs that look like reads: %d\n", len(r.SearchPosts))
	fmt.Fprintf(&b, "GETs: %d list, %d single item, %d other\n", r.ListOps, r.ItemOps, r.OtherOps)
	listable, route, id, none, asserted, proposed := 0, 0, 0, 0, 0, 0
	for _, k := range r.Kinds {
		if len(k.Lists) > 0 {
			listable++
		}
		switch k.KeyFrom {
		case "route":
			route++
		case "id":
			id++
		default:
			none++
		}
		for _, f := range k.Refs {
			if f.Asserted {
				asserted++
			} else {
				proposed++
			}
		}
	}
	fmt.Fprintf(&b, "kinds: %d (%d listable), %d short-name collisions\n", len(r.Kinds), listable, r.Collisions)
	fmt.Fprintf(&b, "keys: %d from the item route, %d from id, %d not guessed\n", route, id, none)
	fmt.Fprintf(&b, "references: %d typed ($ref), %d guessed from *_id names\n", asserted, proposed)
	fmt.Fprintf(&b, "list operations reachable without config: %d of %d\n", r.ReachableLists, r.ListOps)
	fmt.Fprintf(&b, "unresolved path parameters (blocked lists): %s\n", top(r.Unresolved, 8))
	fmt.Fprintf(&b, "paging: %s\n", sortedCounts(r.Paging))
	fmt.Fprintf(&b, "sensitive fields (writeOnly or password): %d\n", r.Sensitive)
	return b.String()
}

func sortedCounts(m map[string]int) string {
	return top(m, len(m))
}

func top(m map[string]int, n int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}
