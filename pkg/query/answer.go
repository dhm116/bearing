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
	// ChangedSince is set on an answer at a past time when today's answer
	// differs: this fact closing, and the facts that opened since from the
	// same start with the same predicate, at the times Bearing recorded.
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
	if len(r.Keys) > 0 {
		out.Key = string(r.Keys[0])
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

// answer asks a at at from the subjects. At a past time it also asks today,
// and each past claim carries what has changed since.
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
	if at.IsZero() {
		return out, nil
	}
	today, err := q.evidence(ctx, subs, a, time.Time{})
	if err != nil {
		return out, err
	}
	then := map[string]bool{}
	for _, c := range past.Claims {
		then[c.Fact.ID] = true
	}
	todayRecs := recordsOf(today)
	nl := q.currentLabeler(time.Time{})
	for i := range out.Claims {
		c := &out.Claims[i]
		if c.Fact.ValidTo != nil {
			c.ChangedSince = append(c.ChangedSince, Change{Kind: string(graph.Closed), At: *c.Fact.ValidTo, Fact: c.Fact})
		}
		for _, n := range today.Claims {
			if then[n.Fact.ID] || !n.Fact.ValidFrom.After(at) || string(n.Start) != c.Start.ID || n.Fact.Predicate != c.Fact.Predicate {
				continue
			}
			f := nl.recordFact(n.Fact, todayRecs)
			c.ChangedSince = append(c.ChangedSince, Change{Kind: string(graph.Opened), At: f.ValidFrom, Fact: f})
		}
	}
	return out, nil
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
	ref := func(id graph.EntityID) Ref { return refOf(recs, id) }
	out := make([]Claim, 0, len(ev.Claims))
	for _, c := range ev.Claims {
		s, ok := start[string(c.Start)]
		if !ok {
			return nil, fmt.Errorf("claim %s: starts at %s, which is no subject", c.Fact.ID, c.Start)
		}
		cl := Claim{Fact: l.recordFact(c.Fact, recs), Start: s, Facts: c.Facts, SameAs: c.SameAs, Path: make([]PathStep, len(c.Path))}
		for i, p := range c.Path {
			step := PathStep{From: ref(p.From), Links: p.Links}
			if p.To != "" {
				to := ref(p.To)
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

func refOf(recs map[graph.EntityID]graph.Record, id graph.EntityID) Ref {
	if r, ok := recs[id]; ok {
		return recordRef(r)
	}
	return Ref{ID: string(id)} // the store returns every record on a path; the ID is still usable
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
func (l *labeler) recordFact(f graph.Fact, recs map[graph.EntityID]graph.Record) Fact {
	sub := refOf(recs, f.Subject)
	var obj *Ref
	if f.Object != "" {
		o := refOf(recs, f.Object)
		obj = &o
	}
	return l.build(f, sub, obj)
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
