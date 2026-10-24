package query

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// MaxRuntimesLimit bounds Runtimes; zero asks for it.
const MaxRuntimesLimit = 1000

// predRuntime is the predicate of a language version a manifest declares: a
// JSON value {language, version, file}.
const predRuntime = "runtime"

// languageAliases maps the names manifests and people use to the one the
// adapters emit.
var languageAliases = map[string]string{"golang": "go", "nodejs": "node"}

// canonLanguage lower-cases a language name and resolves its alias.
func canonLanguage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if a, ok := languageAliases[s]; ok {
		return a
	}
	return s
}

// RuntimesRequest asks which entities declare a language version. The JSON
// names are the API's query parameters.
type RuntimesRequest struct {
	// Language is required, case-insensitive; golang means go and nodejs
	// means node.
	Language string `json:"language"`
	// Below, when set, groups the answer by whether a declared version's
	// lower bound is under it; it must be a version such as 1.22. Empty lists
	// every declaration.
	Below string `json:"below,omitempty"`
	// Kind limits the answer to entities of this kind.
	Kind string `json:"kind,omitempty"`
	// At asks what held then; zero means now.
	At time.Time `json:"at,omitzero"`
	// Limit bounds how many entities each list holds, 1 to MaxRuntimesLimit;
	// zero means MaxRuntimesLimit.
	Limit int `json:"limit,omitempty"`
}

// Declaration is one version an entity declares, with the fact behind it.
type Declaration struct {
	// Version is the declared text, such as ">=18".
	Version string `json:"version"`
	// File is the manifest that declares it.
	File string `json:"file,omitempty"`
	// Lower is the lowest version Version allows; empty when Unknown.
	Lower string `json:"lower,omitempty"`
	// Below says Lower is under the request's Below.
	Below bool `json:"below,omitempty"`
	// Unknown says Version could not be read as a version or range.
	Unknown     bool       `json:"unknown,omitempty"`
	ValidFrom   time.Time  `json:"valid_from"`
	ValidTo     *time.Time `json:"valid_to,omitempty"`
	Sources     []Source   `json:"sources"`
	SourceCount int        `json:"source_count"`
}

// RuntimeEntity is an entity with every declaration of the language.
type RuntimeEntity struct {
	Subject      Ref           `json:"subject"`
	Declarations []Declaration `json:"declarations"`
}

// Runtimes is the answer to Runtimes. With Below set, Below lists the
// entities with a declaration under it, Unknown those with none under it and
// one that could not be read, and AtOrAbove counts the rest. Without Below,
// Entities lists every entity and declaration and the others are empty.
// Truncated says a list was cut at the limit.
type Runtimes struct {
	Language     string          `json:"language"`
	BelowVersion string          `json:"below_version,omitempty"`
	Entities     []RuntimeEntity `json:"entities,omitempty"`
	Below        []RuntimeEntity `json:"below,omitempty"`
	Unknown      []RuntimeEntity `json:"unknown,omitempty"`
	AtOrAbove    int             `json:"at_or_above"`
	Truncated    bool            `json:"truncated,omitempty"`
}

// Runtimes returns the entities that have an asserted runtime fact for a
// language, grouped as RuntimesRequest says. An entity with several
// declarations is below when any of them is, and shows all of them.
func (q *Querier) Runtimes(ctx context.Context, req RuntimesRequest) (*Runtimes, error) {
	lang := canonLanguage(req.Language)
	var below version
	switch {
	case lang == "":
		return nil, fmt.Errorf("%w: a language is required", ErrInvalidRequest)
	case req.Limit < 0 || req.Limit > MaxRuntimesLimit:
		return nil, fmt.Errorf("%w: limit must be from 1 to %d", ErrInvalidRequest, MaxRuntimesLimit)
	case req.Kind != "" && !model.Kind(req.Kind).Valid():
		return nil, fmt.Errorf("%w: kind %q is not a kind", ErrInvalidRequest, model.Clip(req.Kind))
	}
	if req.Below != "" {
		var ok bool
		if below, ok = lowerBound(req.Below); !ok {
			return nil, fmt.Errorf("%w: below %q is not a version", ErrInvalidRequest, model.Clip(req.Below))
		}
	}
	limit := req.Limit
	if limit == 0 {
		limit = MaxRuntimesLimit
	}

	facts, err := q.Graph.Facts(ctx, graph.FactFilter{Predicate: predRuntime, At: req.At})
	if err != nil {
		return nil, fmt.Errorf("runtime facts: %w", err)
	}
	l := q.labeler()
	out := &Runtimes{Language: lang, BelowVersion: req.Below}
	if below != nil {
		out.BelowVersion = below.String()
	}
	// Group by entity, in the order the store returned them.
	bySubject := map[graph.EntityID][]graph.Fact{}
	var order []graph.EntityID
	for _, f := range merge(facts) {
		if _, ok := bySubject[f.Subject]; !ok {
			order = append(order, f.Subject)
		}
		bySubject[f.Subject] = append(bySubject[f.Subject], f)
	}
	type found struct {
		ent                  RuntimeEntity
		anyBelow, anyUnknown bool
	}
	var all []found
	for _, id := range order {
		ref, err := l.ref(ctx, id)
		if err != nil {
			return nil, err
		}
		if req.Kind != "" && ref.Kind != req.Kind {
			continue
		}
		ent := RuntimeEntity{Subject: ref}
		var anyBelow, anyUnknown bool
		for _, f := range bySubject[id] {
			lf, err := l.fact(ctx, f)
			if err != nil {
				return nil, err
			}
			for _, v := range declared(f.Value) {
				if canonLanguage(str(v["language"])) != lang {
					continue
				}
				text := str(v["version"])
				d := Declaration{Version: text, File: str(v["file"]), ValidFrom: lf.ValidFrom, ValidTo: lf.ValidTo, Sources: lf.Sources, SourceCount: lf.SourceCount}
				if low, ok := lowerBound(text); ok {
					d.Lower = low.String()
					d.Below = below != nil && low.compare(below) < 0
				} else {
					d.Unknown = true
				}
				anyBelow, anyUnknown = anyBelow || d.Below, anyUnknown || d.Unknown
				ent.Declarations = append(ent.Declarations, d)
			}
		}
		if len(ent.Declarations) == 0 {
			continue
		}
		slices.SortStableFunc(ent.Declarations, func(a, b Declaration) int {
			return strings.Compare(a.File+"\x00"+a.Version, b.File+"\x00"+b.Version)
		})
		all = append(all, found{ent, anyBelow, anyUnknown})
	}
	slices.SortFunc(all, func(a, b found) int {
		return cmp.Or(strings.Compare(a.ent.Subject.Key, b.ent.Subject.Key), strings.Compare(a.ent.Subject.ID, b.ent.Subject.ID))
	})
	for _, f := range all {
		var list *[]RuntimeEntity
		switch {
		case below == nil:
			list = &out.Entities
		case f.anyBelow:
			list = &out.Below
		case f.anyUnknown:
			list = &out.Unknown
		default:
			out.AtOrAbove++
			continue
		}
		if len(*list) >= limit {
			out.Truncated = true
			continue
		}
		*list = append(*list, f.ent)
	}
	return out, nil
}

// declared returns the declarations in a runtime fact's value: the GitHub
// adapter sends the whole list as one value, which the store keeps as one
// fact (the predicate is not in the core registry, so it is not split).
func declared(v any) []map[string]any {
	var out []map[string]any
	switch v := v.(type) {
	case map[string]any:
		out = append(out, v)
	case []any:
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
