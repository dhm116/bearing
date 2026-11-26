// Package graphview reads the whole store as one picture: every entity with
// its records, the facts between entities and the identity links between
// records, each with its provenance. bearing graph draws it and the API
// serves it at GET /v1/graph.
// shortcut: spike code (spike/graph-svelte); reads everything on every call,
// fine for the Acme demo, not for an org of thousands.
package graphview

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
)

// View is what every format draws: entities (clusters of records), the
// relations between them, and the identity links between records.
type View struct {
	Entities []Entity `json:"entities"`
	Facts    []Fact   `json:"facts"`
	Links    []Link   `json:"links"`
	// At is the time the picture is of; zero means now.
	At time.Time `json:"at,omitzero"`
	// Since is when a source first observed a fact drawn: how far back At
	// can usefully go.
	Since time.Time `json:"since,omitzero"`
}

// Entity is one cluster of records, named by its observed record.
type Entity struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Records []Record `json:"records"`
	// Attributes are the entity's attribute facts, such as language.
	Attributes []Fact `json:"attributes,omitempty"`
}

// Record is one source's record of an entity, with its keys.
type Record struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Label is the key people read: the first readable key, else a stable ID.
	Label string `json:"label"`
	// Keys are the readable keys; StableIDs the opaque ones (GitHub node IDs,
	// UUIDs) that let a record survive a rename.
	Keys      []string `json:"keys"`
	StableIDs []string `json:"stable_ids,omitempty"`
	Sources   []string `json:"sources,omitempty"`
}

// Fact is a relation or attribute and the observations behind it.
type Fact struct {
	ID         string         `json:"id"`
	Subject    string         `json:"subject"`
	Predicate  string         `json:"predicate"`
	Object     string         `json:"object,omitempty"`
	Value      any            `json:"value,omitempty"`
	Qualifiers map[string]any `json:"qualifiers,omitempty"`
	Status     string         `json:"status"`
	// Confidence is between 0 and 1: how sure the sources are of the fact.
	Confidence  float64   `json:"confidence"`
	ValidFrom   time.Time `json:"valid_from"`
	Sources     []Source  `json:"sources"`
	SourceCount int       `json:"source_count"`
}

// Source is one observation behind a fact.
type Source struct {
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	URL        string    `json:"url,omitempty"`
}

// Link is an identity link between two records.
type Link struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Rule      string    `json:"rule"`
	A         string    `json:"a"`
	B         string    `json:"b"`
	ValidFrom time.Time `json:"valid_from"`
	// Strength and Certainty score the link's evidence as the review queue
	// does (pkg/match): how strongly it says "same", and how much of it
	// there is, both between 0 and 1.
	Strength  float64   `json:"strength"`
	Certainty float64   `json:"certainty"`
	Deposits  []Deposit `json:"deposits"`
}

// Deposit is one piece of evidence for or against a link.
type Deposit struct {
	At       time.Time          `json:"at"`
	Weight   float64            `json:"weight"`
	Against  bool               `json:"against,omitempty"`
	Actor    string             `json:"actor,omitempty"`
	Reason   string             `json:"reason,omitempty"`
	Features map[string]float64 `json:"features,omitempty"`
}

// Options say which moment Build reads and how it scores links.
type Options struct {
	// At is the time to read; zero means now.
	At time.Time
	// Now is when link evidence is scored; nil means time.Now.
	Now func() time.Time
	// HalfLife is how fast a kind's link evidence fades; nil means the
	// matching defaults (match.Config{}).
	HalfLife func(model.Kind) time.Duration
}

// Build reads what holds at o.At: every record, every fact open then
// (proposed too) and every link open then. A proposal between records an
// asserted link already joined is left out, as the review queue leaves it
// out: there is nothing left to decide.
// shortcut: clusters and names are today's, so a past picture shows records
// joined or renamed since under their current entity and name; upgrade with
// a store read of records as of a time.
func Build(ctx context.Context, g graph.Graph, o Options) (*View, error) {
	at, halfLife := o.At, o.HalfLife
	var now time.Time
	switch {
	case !at.IsZero():
		now = at // a past link is scored as it stood then
	case o.Now != nil:
		now = o.Now()
	default:
		now = time.Now()
	}
	if halfLife == nil {
		halfLife = match.Config{}.HalfLifeOf
	}
	recs, err := g.Records(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("graphview: %w", err)
	}
	facts, err := g.Facts(ctx, graph.FactFilter{WithProposed: true, At: at})
	if err != nil {
		return nil, fmt.Errorf("graphview: %w", err)
	}
	links, err := g.Links(ctx, graph.LinkFilter{WithClosed: !at.IsZero()})
	if err != nil {
		return nil, fmt.Errorf("graphview: %w", err)
	}
	if !at.IsZero() {
		links = slices.DeleteFunc(links, func(l graph.Link) bool {
			return l.ValidFrom.After(at) || !l.ValidTo.IsZero() && !l.ValidTo.After(at)
		})
	}

	v := &View{Entities: []Entity{}, Facts: []Fact{}, Links: []Link{}, At: at}
	// A past picture is what Bearing knew at at: facts that held then and
	// that a source had observed by then (a fact's valid_from can come from
	// the source and predate Bearing reading it). An entity is drawn if one
	// of those facts names it.
	alive := map[string]bool{}
	known := facts[:0]
	for _, f := range facts {
		first := time.Time{}
		for _, s := range f.Sources {
			if first.IsZero() || s.ObservedAt.Before(first) {
				first = s.ObservedAt
			}
		}
		if !at.IsZero() && (first.IsZero() || first.After(at)) {
			continue
		}
		known = append(known, f)
		alive[string(f.Subject)] = true
		if f.Object != "" {
			alive[string(f.Object)] = true
		}
		if !first.IsZero() && (v.Since.IsZero() || first.Before(v.Since)) {
			v.Since = first
		}
	}
	facts = known
	byCluster := map[string]int{}
	for _, r := range recs {
		if !at.IsZero() && !alive[string(r.Cluster)] {
			continue
		}
		i, ok := byCluster[string(r.Cluster)]
		if !ok {
			i = len(v.Entities)
			byCluster[string(r.Cluster)] = i
			v.Entities = append(v.Entities, Entity{ID: string(r.Cluster), Kind: string(r.Kind), Name: r.Name})
		}
		e := &v.Entities[i]
		// An observed record names the entity; a record only named as a
		// relation's object has no kind and may be the cluster's oldest.
		if e.Kind == "" && r.Kind != "" {
			e.Kind, e.Name = string(r.Kind), r.Name
		}
		e.Records = append(e.Records, newRecord(r))
	}
	for _, f := range facts {
		if f.Predicate == model.PredicateExists {
			continue
		}
		vf := Fact{
			ID: f.ID, Subject: string(f.Subject), Predicate: f.Predicate, Object: string(f.Object),
			Value: f.Value, Qualifiers: f.Qualifiers, Status: string(f.Status), Confidence: f.Confidence, ValidFrom: f.ValidFrom,
			SourceCount: f.SourceCount, Sources: make([]Source, 0, len(f.Sources)),
		}
		for _, s := range f.Sources {
			vf.Sources = append(vf.Sources, Source{Source: s.Source, ObservedAt: s.ObservedAt, URL: s.URL})
		}
		i, ok := byCluster[vf.Subject]
		switch {
		case !ok:
			continue
		case f.Object == "":
			v.Entities[i].Attributes = append(v.Entities[i].Attributes, vf)
		default:
			if _, ok := byCluster[vf.Object]; ok {
				v.Facts = append(v.Facts, vf)
			}
		}
	}
	cluster, kind := map[graph.EntityID]graph.EntityID{}, map[graph.EntityID]model.Kind{}
	for _, r := range recs {
		cluster[r.ID], kind[r.ID] = r.Cluster, r.Kind
	}
	for _, l := range links {
		if l.Kind == graph.SameAs && l.Status == graph.Proposed && cluster[l.A] != "" && cluster[l.A] == cluster[l.B] {
			continue
		}
		sc := match.ScoreOf(l.Deposits, now, halfLife(cmp.Or(kind[l.A], kind[l.B])))
		vl := Link{
			ID: l.ID, Kind: string(l.Kind), Status: string(l.Status), Rule: l.Rule,
			A: string(l.A), B: string(l.B), ValidFrom: l.ValidFrom, Strength: sc.Strength, Certainty: sc.Certainty,
			Deposits: make([]Deposit, 0, len(l.Deposits)),
		}
		for _, d := range l.Deposits {
			vl.Deposits = append(vl.Deposits, Deposit{At: d.At, Weight: d.Weight, Against: d.Against, Actor: d.Actor, Reason: d.Reason, Features: d.Features})
		}
		v.Links = append(v.Links, vl)
	}
	slices.SortFunc(v.Entities, func(a, b Entity) int {
		return cmp.Or(cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return v, nil
}

func newRecord(r graph.Record) Record {
	v := Record{ID: string(r.ID), Name: r.Name, Keys: []string{}, Sources: r.Sources}
	for _, k := range r.Keys {
		if opaqueKey(string(k)) {
			v.StableIDs = append(v.StableIDs, string(k))
		} else {
			v.Keys = append(v.Keys, string(k))
		}
	}
	switch {
	case len(v.Keys) > 0:
		v.Label = v.Keys[0]
	case len(v.StableIDs) > 0: // only an opaque ID: its namespace and the name
		ns, _, _ := strings.Cut(v.StableIDs[0], ":")
		v.Label = ns + ": " + r.Name
	}
	return v
}

// opaqueKey reports whether a key is an ID no person reads: a GitHub node ID
// (github:user_node/U_kgDO…) or one ending in a UUID.
// shortcut: knows only the shapes the bundled adapters emit; a configured
// kind's opaque keys show as readable until adapters mark them.
func opaqueKey(k string) bool {
	typ, rest, _ := strings.Cut(k, "/")
	if strings.HasSuffix(typ, "_node") {
		return true
	}
	last := rest[strings.LastIndex(rest, "/")+1:]
	return len(last) == 36 && strings.Count(last, "-") == 4
}

// kindOrder is the left-to-right order of the core kinds: who, then what they
// own, then what runs. Configured kinds sit after Component.
var kindOrder = []model.Kind{
	model.KindPerson, model.KindTeam, model.KindRepository, model.KindComponent,
	model.KindPackage, model.KindEnvironment, model.KindCloudResource,
}

func kindRank(kind string) int {
	if kind == "" { // records only named as a relation's object, never observed
		return len(kindOrder) + 1
	}
	if i := slices.Index(kindOrder, model.Kind(kind)); i >= 0 {
		if i > 3 {
			return i + 1
		}
		return i
	}
	return 4 // configured kinds
}
