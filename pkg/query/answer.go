package query

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// MaxSubjects bounds the entities a name lookup starts an answer from.
const MaxSubjects = 20

// How a question's subject record matched the reference it was asked with
// (ADR 11).
const (
	// MatchKey means the reference is the record's key or ID.
	MatchKey = "key"
	// MatchName means the record's name is the reference, ignoring case.
	MatchName = "name"
	// MatchFuzzy means the name or a key only resembles it; Score says how
	// closely.
	MatchFuzzy = "fuzzy"
)

// How two clusters at the ends of claims relate (ADR 11), first rule first.
const (
	// RelationDistinct means an asserted distinct_from says they are not the
	// same.
	RelationDistinct = "distinct"
	// RelationAssertedNotJoined means an asserted same_as names them as one,
	// but the store did not join them; Reason says why.
	RelationAssertedNotJoined = "asserted_not_joined"
	// RelationLikelySame and RelationLikelyDistinct mean only proposals
	// relate them, the strongest a same_as or a distinct_from.
	RelationLikelySame     = "likely_same"
	RelationLikelyDistinct = "likely_distinct"
)

// Why an asserted same_as did not join its records.
const (
	// ReasonKinds means the records' kinds are on different lines of descent.
	ReasonKinds = "kinds_differ"
	// ReasonDistinct means an asserted distinct_from keeps the clusters apart.
	ReasonDistinct = "distinct_from"
)

// Subject is a record a question starts from, and how it matched.
type Subject struct {
	Ref
	// Match is MatchKey, MatchName or MatchFuzzy.
	Match string `json:"match"`
	// Score is 1 for a key or a name, and between 0 and 1 when fuzzy.
	Score float64 `json:"score"`
}

// Answer is what the owners and related questions share: the records they
// start from, a claim per asserted fact, and how the claims' ends relate.
type Answer struct {
	Subjects []Subject `json:"subjects"`
	// Claims are never merged or chosen between; ADR 11 orders them.
	Claims []Claim `json:"claims"`
	// ChangedSince is set on an answer at a past time: the facts today's
	// answer has that opened since, at their valid_from. A claim that closed
	// since carries its own closing.
	ChangedSince []Change `json:"changed_since,omitempty"`
	// Relations are ordered by cluster.
	Relations []Relation `json:"relations"`
	// LimitHit says the name lookup found more than MaxSubjects entities,
	// and only the best are subjects.
	LimitHit bool `json:"limit_hit,omitempty"`
}

// Claim is one asserted fact, its sources, and the path to it from a
// subject record.
type Claim struct {
	// Fact names records: its subject and object carry their clusters.
	Fact Fact `json:"fact"`
	// Start is the subject record the path starts at.
	Start Subject `json:"start"`
	// Path is a same_as hop (at most one) before each fact, then the fact.
	Path []PathStep `json:"path"`
	// Facts and SameAs count the facts and hops on Path.
	Facts  int `json:"facts"`
	SameAs int `json:"same_as"`
	// ChangedSince is set on an answer at a past time when this fact has
	// closed since: one closed change, at its valid_to.
	ChangedSince []Change `json:"changed_since,omitempty"`
}

// PathStep is a same_as hop between two records of one cluster, or a fact
// crossed from one record to the next.
type PathStep struct {
	From Ref `json:"from"`
	// To is empty for an attribute fact.
	To *Ref `json:"to,omitempty"`
	// FactID and Predicate are set on a fact step; FactID is a Fact's ID.
	FactID    string `json:"fact_id,omitempty"`
	Predicate string `json:"predicate,omitempty"`
	// Links are a hop's asserted same_as link IDs, From to To: the ones the
	// store applied to join the records.
	Links []string `json:"links,omitempty"`
}

// Relation says once how two clusters among the claims' objects relate.
type Relation struct {
	// Clusters are the two cluster IDs, smaller first; a claim's object
	// carries its cluster.
	Clusters [2]string `json:"clusters"`
	Kind     string    `json:"kind"`
	// Reason says why an asserted same_as did not join them.
	Reason string `json:"reason,omitempty"`
	// Links are the open links between their records: the deciding ones
	// first, then proposals, strongest first, scored at the answer's time.
	Links []Link `json:"links"`
}

// subjects resolves a reference to the records a question starts from: the
// record a key or ID names, or else, for a reference that is not shaped like
// a key (namespace:path), the records a name lookup finds.
func (q *Querier) subjects(ctx context.Context, ref string) ([]Subject, bool, error) {
	if ref == "" {
		return nil, false, fmt.Errorf("%w: a reference is required", ErrInvalidRequest)
	}
	e, err := q.Graph.Entity(ctx, ref)
	switch {
	case err == nil:
		for _, r := range e.Records {
			if string(r.ID) == ref || slices.Contains(r.Keys, model.Key(ref)) {
				return []Subject{{Ref: recordRef(r), Match: MatchKey, Score: 1}}, false, nil
			}
		}
		return nil, false, fmt.Errorf("entity %s: no record names it: %w", ref, ErrNotFound)
	case !errors.Is(err, graph.ErrNotFound):
		return nil, false, fmt.Errorf("entity %s: %w", ref, err)
	case len(ref) > MaxFindText || strings.Contains(ref, ":"):
		// A key names one record or none; only a name is looked up.
		return nil, false, fmt.Errorf("entity %s: %w", model.Clip(ref), ErrNotFound)
	}
	ms, err := q.Graph.Find(ctx, ref, "", MaxSubjects+1)
	if err != nil {
		return nil, false, fmt.Errorf("find %s: %w", ref, err)
	}
	hit := len(ms) > MaxSubjects
	var out []Subject
	for _, m := range ms[:min(len(ms), MaxSubjects)] {
		for _, h := range m.Records {
			i := slices.IndexFunc(m.Entity.Records, func(r graph.Record) bool { return r.ID == h.ID })
			if i < 0 {
				continue
			}
			s := Subject{Ref: recordRef(m.Entity.Records[i]), Match: MatchFuzzy, Score: h.Score}
			if strings.EqualFold(s.Name, ref) {
				s.Match, s.Score = MatchName, 1
			}
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, false, fmt.Errorf("entity %s: %w", ref, ErrNotFound)
	}
	return out, hit, nil
}

// recordRef names a record and its cluster.
func recordRef(r graph.Record) Ref {
	out := Ref{ID: string(r.ID), Cluster: string(r.Cluster), Kind: string(r.Kind), Name: r.Name}
	for _, k := range r.Keys {
		out.Keys = append(out.Keys, string(k))
	}
	if len(out.Keys) > 0 {
		out.Key = out.Keys[0]
	}
	return out
}

// ask is one question over records: the predicate sets to try in turn (the
// first that yields a claim answers), which way, how deep.
type ask struct {
	preds   [][]string
	reverse bool
	depth   int
}

// ownerAsk is Q1: owned_by answers first, approves_changes (CODEOWNERS) when
// no subject record reaches an owned_by fact.
var ownerAsk = ask{preds: [][]string{{predOwnedBy}, {predApprovesChanges}}, depth: 1}

// answer asks a at at from the subjects. At a past time it also asks today:
// each past claim that closed since carries its closing, and the answer
// carries the facts today's answer has that opened since.
func (q *Querier) answer(ctx context.Context, subs []Subject, a ask, at time.Time) (Answer, error) {
	out := Answer{Subjects: subs}
	past, err := q.evidence(ctx, subs, a, at)
	if err != nil {
		return out, err
	}
	l := q.currentLabeler(at)
	recs := recordsOf(past)
	if out.Claims, err = claims(l, past, subs, recs); err != nil {
		return out, err
	}
	if out.Relations, err = q.relations(past, recs, at); err != nil {
		return out, err
	}
	refs := claimRefs(out.Claims)
	if !at.IsZero() {
		today, err := q.evidence(ctx, subs, a, time.Time{})
		if err != nil {
			return out, err
		}
		then := map[string]bool{}
		for _, c := range past.Claims {
			then[c.Fact.ID] = true
		}
		for i := range out.Claims {
			if c := &out.Claims[i]; c.Fact.ValidTo != nil {
				c.ChangedSince = []Change{{Kind: string(graph.Closed), At: *c.Fact.ValidTo, Fact: c.Fact}}
			}
		}
		todayRecs := recordsOf(today)
		nl := q.currentLabeler(time.Time{})
		for _, n := range today.Claims {
			if then[n.Fact.ID] || !n.Fact.ValidFrom.After(at) {
				continue
			}
			f, err := nl.recordFact(n.Fact, todayRecs)
			if err != nil {
				return out, err
			}
			out.ChangedSince = append(out.ChangedSince, Change{Kind: string(graph.Opened), At: f.ValidFrom, Fact: f})
		}
		for i := range out.ChangedSince {
			refs = append(refs, factRefs(&out.ChangedSince[i].Fact)...)
		}
	}
	return out, q.clusterKinds(ctx, refs)
}

// claimRefs are the record references of claims, for clusterKinds.
func claimRefs(cs []Claim) []*Ref {
	var out []*Ref
	for i := range cs {
		c := &cs[i]
		out = append(out, &c.Start.Ref)
		out = append(out, factRefs(&c.Fact)...)
		for j := range c.Path {
			out = append(out, &c.Path[j].From)
			if c.Path[j].To != nil {
				out = append(out, c.Path[j].To)
			}
		}
	}
	return out
}

func factRefs(f *Fact) []*Ref {
	if f.Object == nil {
		return []*Ref{&f.Subject}
	}
	return []*Ref{&f.Subject, f.Object}
}

// clusterKinds sets ClusterKind on the kindless records among refs, one read
// per cluster.
func (q *Querier) clusterKinds(ctx context.Context, refs []*Ref) error {
	kinds := map[string]string{}
	for _, r := range refs {
		if r.Kind != "" || r.Cluster == "" {
			continue
		}
		k, ok := kinds[r.Cluster]
		if !ok {
			e, err := q.Graph.Entity(ctx, r.Cluster)
			if err != nil {
				return fmt.Errorf("cluster %s: %w", r.Cluster, err)
			}
			k = string(e.Kind)
			kinds[r.Cluster] = k
		}
		r.ClusterKind = k
	}
	return nil
}

// evidence reads the claims of a, trying each predicate set in turn.
func (q *Querier) evidence(ctx context.Context, subs []Subject, a ask, at time.Time) (graph.Evidence, error) {
	from := make([]graph.EntityID, len(subs))
	for i, s := range subs {
		from[i] = graph.EntityID(s.ID)
	}
	var ev graph.Evidence
	for _, preds := range a.preds {
		var err error
		ev, err = q.Graph.Evidence(ctx, graph.EvidenceRequest{From: from, Predicates: preds, Reverse: a.reverse, MaxDepth: a.depth, At: at})
		if err != nil {
			return ev, fmt.Errorf("evidence: %w", err)
		}
		// Owners and related answer relations; an attribute is no claim.
		ev.Claims = slices.DeleteFunc(ev.Claims, func(c graph.Claim) bool { return c.Fact.Object == "" })
		if len(ev.Claims) > 0 {
			break
		}
	}
	return ev, nil
}

func recordsOf(ev graph.Evidence) map[graph.EntityID]graph.Record {
	out := make(map[graph.EntityID]graph.Record, len(ev.Records))
	for _, r := range ev.Records {
		out[r.ID] = r
	}
	return out
}

// claims turns the store's claims into the answer's, in ADR 11's order:
// fewer facts, fewer same_as hops, the smallest source name, the object
// record's first key, the subject record's first key, then fact ID.
func claims(l *labeler, ev graph.Evidence, subs []Subject, recs map[graph.EntityID]graph.Record) ([]Claim, error) {
	start := make(map[string]Subject, len(subs))
	for _, s := range subs {
		start[s.ID] = s
	}
	out := make([]Claim, 0, len(ev.Claims))
	for _, c := range ev.Claims {
		s, ok := start[string(c.Start)]
		if !ok {
			return nil, fmt.Errorf("claim %s: starts at %s, which is no subject", c.Fact.ID, c.Start)
		}
		f, err := l.recordFact(c.Fact, recs)
		if err != nil {
			return nil, err
		}
		cl := Claim{Fact: f, Start: s, Facts: c.Facts, SameAs: c.SameAs, Path: make([]PathStep, len(c.Path))}
		for i, p := range c.Path {
			from, err := refOf(recs, p.From)
			if err != nil {
				return nil, err
			}
			step := PathStep{From: from, Links: p.Links}
			if p.To != "" {
				to, err := refOf(recs, p.To)
				if err != nil {
					return nil, err
				}
				step.To = &to
			}
			if p.Fact != nil {
				step.FactID, step.Predicate = p.Fact.ID, p.Fact.Predicate
			}
			cl.Path[i] = step
		}
		out = append(out, cl)
	}
	slices.SortStableFunc(out, func(a, b Claim) int {
		return cmp.Or(cmp.Compare(a.Facts, b.Facts), cmp.Compare(a.SameAs, b.SameAs),
			cmp.Compare(firstSource(a.Fact), firstSource(b.Fact)),
			cmp.Compare(objectKey(a.Fact), objectKey(b.Fact)),
			cmp.Compare(a.Fact.Subject.Key, b.Fact.Subject.Key),
			cmp.Compare(a.Fact.ID, b.Fact.ID))
	})
	return out, nil
}

// firstSource is the smallest name among a fact's sources.
func firstSource(f Fact) string {
	var s string
	for i, src := range f.Sources {
		if i == 0 || src.Source < s {
			s = src.Source
		}
	}
	return s
}

func objectKey(f Fact) string {
	if f.Object == nil {
		return ""
	}
	return f.Object.Key
}

// refOf names a record the store returned. A missing one is a store bug:
// Evidence returns every record on its paths, and records are never deleted.
func refOf(recs map[graph.EntityID]graph.Record, id graph.EntityID) (Ref, error) {
	if r, ok := recs[id]; ok {
		return recordRef(r), nil
	}
	return Ref{}, fmt.Errorf("record %s: not returned by the store", id)
}

// relations words the store's relations: the strongest proposal decides
// between likely the same and likely not, and links are scored at the
// answer's time.
func (q *Querier) relations(ev graph.Evidence, recs map[graph.EntityID]graph.Record, at time.Time) ([]Relation, error) {
	out := make([]Relation, 0, len(ev.Relations))
	sc := q.scorer(at)
	record := func(id graph.EntityID) LinkRecord { return linkRecord(recs, id) }
	for _, r := range ev.Relations {
		rel := Relation{Clusters: [2]string{string(r.A), string(r.B)}, Links: make([]Link, 0, len(r.Links))}
		deciding := 0
		for _, l := range r.Links {
			rel.Links = append(rel.Links, sc.link(l, record))
			if l.Status == graph.Asserted {
				deciding++
			}
		}
		proposals := rel.Links[deciding:]
		slices.SortStableFunc(proposals, func(x, y Link) int { return cmp.Compare(y.Score.Strength, x.Score.Strength) })
		switch r.Kind {
		case graph.RelationDistinct:
			rel.Kind = RelationDistinct
		case graph.RelationAssertedNotJoined:
			rel.Kind, rel.Reason = RelationAssertedNotJoined, ReasonDistinct
			for _, l := range r.Links[:deciding] {
				ka, kb := recs[l.A].Kind, recs[l.B].Kind
				if ka != "" && kb != "" && !q.Registry.OneLine(ka, kb) {
					rel.Reason = ReasonKinds
				}
			}
		case graph.RelationProposed:
			rel.Kind = RelationLikelySame
			if len(proposals) > 0 && proposals[0].Kind == string(graph.DistinctFrom) {
				rel.Kind = RelationLikelyDistinct
			}
		default:
			return nil, fmt.Errorf("relation %s %s: unknown kind %q", r.A, r.B, r.Kind)
		}
		out = append(out, rel)
	}
	return out, nil
}

// recordFact is a fact whose subject and object name records.
func (l *labeler) recordFact(f graph.Fact, recs map[graph.EntityID]graph.Record) (Fact, error) {
	sub, err := refOf(recs, f.Subject)
	if err != nil {
		return Fact{}, fmt.Errorf("fact %s: %w", f.ID, err)
	}
	var obj *Ref
	if f.Object != "" {
		o, err := refOf(recs, f.Object)
		if err != nil {
			return Fact{}, fmt.Errorf("fact %s: %w", f.ID, err)
		}
		obj = &o
	}
	return l.build(f, sub, obj), nil
}

// recordsByID loads the records with these IDs, for answers that name
// records outside a claim's path (changes, runtimes).
func (q *Querier) recordsByID(ctx context.Context, ids []graph.EntityID) (map[graph.EntityID]graph.Record, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return map[graph.EntityID]graph.Record{}, nil
	}
	got, err := q.Graph.RecordsByID(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("records: %w", err)
	}
	out := make(map[graph.EntityID]graph.Record, len(got))
	for _, r := range got {
		out[r.ID] = r
	}
	return out, nil
}
