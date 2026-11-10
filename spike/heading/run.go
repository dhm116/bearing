package heading

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

// Mapping is what an author would keep in heading.yaml: everything the
// spec doesn't say. A generated adapter would compile it in; the spike
// interprets it.
type Mapping struct {
	// Namespace is the kinds' namespace and the keys' issuer.
	Namespace string
	// Instance prefixes every key, so two clusters (or orgs) never collide.
	Instance string
	Kinds    []KindMap
}

// KindMap maps one kind. Kinds that fill another's path parameters come first.
type KindMap struct {
	// Kind is the bare kind name; the namespace is added.
	Kind string
	// Schema is the component the items follow, for flattening.
	Schema string
	// List is the list route; {param} values come from Params.
	List   string
	Params map[string]From
	// Items is the field holding the list; empty when the body is an array.
	Items string
	Key   []string
	// Cursor is the continue-style paging: the query parameter to send and
	// the response field that holds the next value.
	Cursor *Cursor
	// Attributes are projections the flattening doesn't reach.
	Attributes map[string]string
	Refs       []RefMap
}

// From fills a path parameter from a field of every item of an earlier kind.
type From struct{ Kind, Field string }

// Cursor is continue-style paging.
type Cursor struct{ Param, Field string }

// RefMap makes a field's value a relation to the kind whose key it is.
type RefMap struct{ Predicate, Field, Kind string }

// Runner runs a mapping against an API, GET only.
type Runner struct {
	HTTP   *http.Client
	Base   string
	Token  string
	Now    func() time.Time
	Source string
	Spec   *Spec
}

// Run lists every kind of m and returns one observation per item.
func (r *Runner) Run(ctx context.Context, m Mapping) ([]*eventv1alpha1.Observation, error) {
	seen := map[string][]map[string]any{}
	var out []*eventv1alpha1.Observation
	for _, km := range m.Kinds {
		for _, path := range expand(km, seen) {
			next := ""
			for page := 0; page < 1000; page++ {
				body, at, err := r.get(ctx, path, km.Cursor, next)
				if err != nil {
					return nil, fmt.Errorf("heading: %s: %w", km.Kind, err)
				}
				items := body
				if km.Items != "" {
					items = lookup(body, km.Items)
				}
				for _, it := range arr(items) {
					item := obj(it)
					seen[km.Kind] = append(seen[km.Kind], item)
					out = append(out, r.observation(m, km, item, at))
				}
				if km.Cursor == nil {
					break
				}
				if next = str(lookup(body, km.Cursor.Field)); next == "" {
					break
				}
			}
		}
	}
	return out, nil
}

// expand fills a route's parameters from the items seen so far, one path
// per combination (the spike handles one parameter per route).
func expand(km KindMap, seen map[string][]map[string]any) []string {
	paths := []string{km.List}
	for param, from := range km.Params {
		var next []string
		for _, p := range paths {
			for _, it := range seen[from.Kind] {
				next = append(next, strings.ReplaceAll(p, "{"+param+"}", url.PathEscape(str(lookup(it, from.Field)))))
			}
		}
		paths = next
	}
	return paths
}

func (r *Runner) get(ctx context.Context, path string, c *Cursor, next string) (any, time.Time, error) {
	u := r.Base + path
	if c != nil && next != "" {
		u += "?" + url.Values{c.Param: {next}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	at := r.Now()
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, at, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, at, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	var body any
	err = json.NewDecoder(resp.Body).Decode(&body)
	return body, at, err
}

func (r *Runner) observation(m Mapping, km KindMap, item map[string]any, at time.Time) *eventv1alpha1.Observation {
	attrs := map[string]*structpb.Value{}
	_, sch := r.Spec.deref(componentRef(km.Schema))
	r.flatten(sch, item, "", 0, attrs)
	for name, path := range km.Attributes {
		if v := project(item, path); len(v) > 0 {
			attrs[name], _ = structpb.NewValue(v)
		}
	}
	var rels []*modelv1alpha1.Relation
	for _, ref := range km.Refs {
		if v := str(lookup(item, ref.Field)); v != "" {
			rels = append(rels, &modelv1alpha1.Relation{Type: ref.Predicate, End: &modelv1alpha1.Relation_To{To: m.key(ref.Kind, v)}})
		}
	}
	return model.NewObservation(r.Source, at, &modelv1alpha1.ObservationData{
		Entity:    &modelv1alpha1.Entity{Kind: m.Namespace + "." + km.Kind, Key: m.key(km.Kind, keyOf(item, km.Key)), Attributes: attrs},
		Relations: rels,
	})
}

func (m Mapping) key(kind, id string) string {
	return string(model.NewKey(m.Namespace, strings.ToLower(kind), m.Instance+"/"+id))
}

func keyOf(item map[string]any, fields []string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprint(lookup(item, f))
	}
	return strings.Join(parts, "/")
}

// flatten turns an item into attributes, guided by its schema: fields with
// declared properties are walked two levels deep and named by their path
// (metadata.creationTimestamp becomes metadata_creation_timestamp); maps,
// arrays of objects and anything deeper are kept whole as JSON.
func (r *Runner) flatten(sch map[string]any, v any, prefix string, depth int, out map[string]*structpb.Value) {
	props := obj(sch["properties"])
	for name, val := range obj(v) {
		attr := prefix + snake(name)
		_, ps := r.Spec.deref(props[name])
		switch {
		case val == nil:
		case ps["properties"] != nil && depth < 2:
			r.flatten(ps, val, attr+"_", depth+1, out)
		default:
			if pv, err := structpb.NewValue(val); err == nil {
				out[attr] = pv
			}
		}
	}
}

var camel = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func snake(s string) string { return strings.ToLower(camel.ReplaceAllString(s, "${1}_${2}")) }

// lookup reads a dotted field path.
func lookup(v any, path string) any {
	for _, p := range strings.Split(path, ".") {
		v = obj(v)[p]
	}
	return v
}

// project reads a path with [*] steps into a flat list of scalars.
func project(v any, path string) []any {
	cur := []any{v}
	for _, p := range strings.Split(path, ".") {
		var next []any
		name, all := strings.CutSuffix(p, "[*]")
		for _, c := range cur {
			x := obj(c)[name]
			if all {
				next = append(next, arr(x)...)
			} else if x != nil {
				next = append(next, x)
			}
		}
		cur = next
	}
	return cur
}
