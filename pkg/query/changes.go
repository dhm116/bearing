package query

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"bearing.example/pkg/graph"
)

// Change is a fact that was opened or closed At.
type Change struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Fact Fact      `json:"fact"`
}

// Changes is the answer to Changes, newest first: the facts opened or
// closed in the window, per record, and the asserted joins and splits.
type Changes struct {
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Changes []Change  `json:"changes"`
	// Identity are the joins and splits of clusters in the window, one per
	// decision; proposals never appear.
	Identity []IdentityChange `json:"identity"`
}

// The kinds of IdentityChange.
const (
	// IdentityJoined means two clusters became one: an asserted same_as
	// opened, or an asserted distinct_from closed.
	IdentityJoined = "joined"
	// IdentitySplit means one cluster became two: an asserted same_as closed,
	// or an asserted distinct_from opened.
	IdentitySplit = "split"
	// IdentityNotJoined means an asserted same_as opened but the store did
	// not apply it (a kind line or a distinct_from).
	IdentityNotJoined = "asserted_not_joined"
)

// IdentityChange is one decision that joined or split clusters: the link
// rows it opened or closed at one time, and why.
type IdentityChange struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	// A and B are the records of the row that decided the kind.
	A LinkRecord `json:"a"`
	B LinkRecord `json:"b"`
	// Links are every asserted row the decision opened or closed.
	Links         []LinkRow     `json:"links"`
	Justification Justification `json:"justification"`
}

// LinkRow is one identity link row an IdentityChange opened or closed.
type LinkRow struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Change is "opened" or "closed"; Effect is what that did to the
	// clusters, as the store recorded it then: joined, split, none or refused.
	Change string `json:"change"`
	Effect string `json:"effect"`
	A      string `json:"a"`
	B      string `json:"b"`
	// PromotedFrom is the proposal this asserted row promoted, whose
	// matcher evidence stays with it.
	PromotedFrom string `json:"promoted_from,omitempty"`
}

// Justification is why a join or split happened: the source observation, the
// settings rule, or a person's decision.
type Justification struct {
	// Rule names what made the link: linked_ids or shared_key (a source's
	// observation), a settings rule, or "person".
	Rule          string `json:"rule"`
	ObservationID string `json:"observation_id,omitempty"`
	// Decision is the person's decision (its event ID), with Actor and
	// Reason when the decision opened a row.
	Decision string `json:"decision,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Changes returns the facts opened or closed in [since, until), about the
// entity ref names or, when ref is empty, about anything, and the asserted
// joins and splits in the window (ADR 11). until defaults to now and since to
// 24 hours before until; limit bounds each list, zero meaning the store's
// default.
func (q *Querier) Changes(ctx context.Context, ref string, since, until time.Time, limit int) (*Changes, error) {
	if limit < 0 || limit > MaxChangesLimit {
		return nil, fmt.Errorf("%w: limit must be from 1 to %d", ErrInvalidRequest, MaxChangesLimit)
	}
	if until.IsZero() {
		now := time.Now
		if q.Now != nil {
			now = q.Now
		}
		until = now().UTC()
	}
	if since.IsZero() {
		since = until.Add(-defaultWindow)
	}
	if !since.Before(until) {
		return nil, fmt.Errorf("%w: the window starts at or after its end", ErrInvalidRequest)
	}
	req := graph.ChangesRequest{Since: since, Until: until, Limit: limit}
	lf := graph.LinkFilter{Status: graph.Asserted, ChangedSince: since, ChangedUntil: until}
	if ref != "" {
		e, err := q.entity(ctx, ref)
		if err != nil {
			return nil, err
		}
		req.Subject, lf.Entity = e.ID, e.ID
	}
	changes, err := q.Graph.Changes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	links, err := q.Graph.Links(ctx, lf)
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	var ids []graph.EntityID
	for _, c := range changes {
		ids = append(ids, c.Fact.Subject, c.Fact.Object)
	}
	for _, l := range links {
		ids = append(ids, l.A, l.B)
	}
	recs, err := q.recordsByID(ctx, slices.DeleteFunc(ids, func(id graph.EntityID) bool { return id == "" }))
	if err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	l := q.labeler()
	out := &Changes{Since: since, Until: until, Changes: make([]Change, 0, len(changes))}
	for _, c := range changes {
		out.Changes = append(out.Changes, Change{Kind: string(c.Kind), At: c.At, Fact: l.recordFact(c.Fact, recs)})
	}
	// The store breaks ties at one instant on random IDs; order them by what
	// they say so the same history always reads the same.
	slices.SortStableFunc(out.Changes, func(a, b Change) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Fact.Subject.Key, b.Fact.Subject.Key), cmp.Compare(a.Fact.Predicate, b.Fact.Predicate),
			cmp.Compare(objectText(a.Fact), objectText(b.Fact)))
	})
	out.Identity = identityChanges(links, recs, since, until)
	if limit == 0 {
		limit = MaxChangesLimit
	}
	out.Identity = out.Identity[:min(limit, len(out.Identity))]
	return out, nil
}

// objectText is what a fact says about its subject, for ordering.
func objectText(f Fact) string {
	if f.Object != nil {
		return f.Object.Key
	}
	return fmt.Sprint(f.Value)
}

// identityChanges groups the asserted rows that opened or closed in the
// window into one event per decision: by the person's decision ID, or else
// by pair and time. A group is listed when a row in it joined, split, or was
// refused, newest first.
func identityChanges(links []graph.Link, recs map[graph.EntityID]graph.Record, since, until time.Time) []IdentityChange {
	in := func(t time.Time) bool { return !t.IsZero() && !t.Before(since) && t.Before(until) }
	var rows []row
	for _, l := range links {
		if in(l.ValidFrom) {
			rows = append(rows, row{l, false, l.ValidFrom, l.OpenDecision, l.OpenEffect})
		}
		if in(l.ValidTo) {
			rows = append(rows, row{l, true, l.ValidTo, l.CloseDecision, l.CloseEffect})
		}
	}
	type key struct {
		decision, a, b string
		at             time.Time
	}
	keyOf := func(r row) key {
		if r.decision != "" {
			return key{decision: r.decision}
		}
		return key{a: string(r.l.A), b: string(r.l.B), at: r.at}
	}
	out := []IdentityChange{}
	for _, group := range groupBy(rows, keyOf) {
		var ev IdentityChange
		var decider *row
		rank := map[graph.LinkEffect]int{graph.EffectJoined: 3, graph.EffectSplit: 2, graph.EffectRefused: 1}
		for i, r := range group {
			change := string(graph.Opened)
			if r.closing {
				change = string(graph.Closed)
			}
			ev.Links = append(ev.Links, LinkRow{
				ID: r.l.ID, Kind: string(r.l.Kind), Change: change, Effect: string(r.effect),
				A: string(r.l.A), B: string(r.l.B), PromotedFrom: r.l.PromotedFrom,
			})
			if rank[r.effect] > 0 && (decider == nil || rank[r.effect] > rank[decider.effect]) {
				decider = &group[i]
			}
		}
		if decider == nil {
			continue // nothing joined or split: a reinforcement, or a row that changed no cluster
		}
		ev.Kind = map[graph.LinkEffect]string{graph.EffectJoined: IdentityJoined, graph.EffectSplit: IdentitySplit, graph.EffectRefused: IdentityNotJoined}[decider.effect]
		ev.At, ev.A, ev.B = decider.at, linkRecord(recs, decider.l.A), linkRecord(recs, decider.l.B)
		ev.Justification = justify(group)
		out = append(out, ev)
	}
	slices.SortStableFunc(out, func(x, y IdentityChange) int {
		return cmp.Or(y.At.Compare(x.At), cmp.Compare(x.Kind, y.Kind), cmp.Compare(x.A.Key, y.A.Key), cmp.Compare(x.B.Key, y.B.Key))
	})
	return out
}

// row is one link row opening or closing at a time.
type row struct {
	l        graph.Link
	closing  bool
	at       time.Time
	decision string
	effect   graph.LinkEffect
}

// justify is the reason for one event: the deposit an opened row opened
// with (the observation, or the person and their reason), and the rule of
// that row, or of a closed one when nothing opened.
func justify(rows []row) Justification {
	j := Justification{Decision: rows[0].decision}
	for _, r := range rows {
		l := r.l
		if j.Rule == "" {
			j.Rule = l.Rule
		}
		if r.closing {
			continue
		}
		j.Rule = l.Rule
		// An asserted row's first deposit is the one it opened with.
		if len(l.Deposits) > 0 {
			d := l.Deposits[0]
			j.ObservationID, j.Actor, j.Reason = d.ObservationID, d.Actor, d.Reason
		}
		break
	}
	return j
}
