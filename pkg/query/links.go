package query

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
)

// Limits of the review queue.
const (
	// MaxLinksLimit bounds Links.
	MaxLinksLimit = 1000
	// DefaultLinksLimit is the number of links Links returns when asked for
	// none in particular.
	DefaultLinksLimit = 100
	// linksScanCap bounds the links Links scores, those with the newest
	// evidence; the strongest are among them, since evidence decays.
	//
	// shortcut: a stale strong link past the cap is left out; push the score
	// into the store if a queue ever holds that many.
	linksScanCap = 5 * MaxLinksLimit
)

// LinksRequest selects the identity links Links returns. Empty fields match
// anything; only open links are returned.
type LinksRequest struct {
	// Status is "proposed" or "asserted"; the review queue is "proposed".
	Status string
	// Kind is "same_as" or "distinct_from".
	Kind string
	// Entity limits the links to those touching a record of the entity (an
	// entity ID, a record ID or a key).
	Entity string
	// Limit bounds the answer; zero means DefaultLinksLimit.
	Limit int
}

// LinkRecord is one end of a link: the record as its source described it.
type LinkRecord struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	// Key is the record's first key, sorted.
	Key string `json:"key,omitempty"`
	// Sources are the configured sources that observed the record.
	Sources []string `json:"sources,omitempty"`
}

// LinkScore is a link's evidence at the time of the question (pkg/match).
type LinkScore struct {
	// Strength is between 0 and 1; the queue is ordered by it.
	Strength float64 `json:"strength"`
	// Certainty is how much evidence there is, between 0 and 1.
	Certainty float64 `json:"certainty"`
	// Support and Contrary are the decayed weights of the deposits for and
	// against the link.
	Support  float64 `json:"support"`
	Contrary float64 `json:"contrary"`
}

// LinkDeposit is one piece of evidence behind a link.
type LinkDeposit struct {
	At      time.Time `json:"at"`
	Weight  float64   `json:"weight"`
	Against bool      `json:"against,omitempty"`
	// ObservationID is the observation behind it, for evidence from a source.
	ObservationID string `json:"observation_id,omitempty"`
	// Features are a matcher's inputs and their contributions.
	Features map[string]float64 `json:"features,omitempty"`
	// Actor and Reason are who decided and why, for a person's decision.
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Link is an identity link in the form a person reviews it.
type Link struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	// Rule names what made the link: a key rule, a matcher or "person".
	Rule      string        `json:"rule"`
	A         LinkRecord    `json:"a"`
	B         LinkRecord    `json:"b"`
	Score     LinkScore     `json:"score"`
	Deposits  []LinkDeposit `json:"deposits"`
	ValidFrom time.Time     `json:"valid_from"`
}

// Links returns the open identity links req selects, strongest first: the
// queue a person works through. Each carries both records and its evidence,
// scored at the querier's time.
func (q *Querier) Links(ctx context.Context, req LinksRequest) ([]Link, error) {
	f := graph.LinkFilter{Kind: graph.LinkKind(req.Kind), Status: graph.Status(req.Status)}
	switch {
	case req.Kind != "" && f.Kind != graph.SameAs && f.Kind != graph.DistinctFrom:
		return nil, fmt.Errorf("%w: kind must be same_as or distinct_from", ErrInvalidRequest)
	case req.Status != "" && f.Status != graph.Proposed && f.Status != graph.Asserted:
		return nil, fmt.Errorf("%w: status must be proposed or asserted", ErrInvalidRequest)
	case req.Limit < 0 || req.Limit > MaxLinksLimit:
		return nil, fmt.Errorf("%w: limit must be from 1 to %d", ErrInvalidRequest, MaxLinksLimit)
	}
	if req.Entity != "" {
		if _, err := q.entity(ctx, req.Entity); err != nil {
			return nil, err
		}
	}
	f.Entity, f.Limit = graph.EntityID(req.Entity), linksScanCap
	ls, err := q.Graph.Links(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	now := time.Now
	if q.Now != nil {
		now = q.Now
	}
	halfLife := q.HalfLife
	if halfLife == nil {
		halfLife = match.Config{}.HalfLifeOf
	}
	at := now()
	var ids []graph.EntityID
	for _, l := range ls {
		ids = append(ids, l.A, l.B)
	}
	slices.Sort(ids)
	got, err := q.Graph.RecordsByID(ctx, slices.Compact(ids))
	if err != nil {
		return nil, fmt.Errorf("links: records: %w", err)
	}
	recs := map[graph.EntityID]LinkRecord{}
	cluster := map[graph.EntityID]graph.EntityID{}
	for _, x := range got {
		cluster[x.ID] = x.Cluster
		r := LinkRecord{ID: string(x.ID), Kind: string(x.Kind), Name: x.Name, Sources: x.Sources}
		if len(x.Keys) > 0 {
			r.Key = string(x.Keys[0])
		}
		recs[x.ID] = r
	}
	record := func(id graph.EntityID) LinkRecord {
		if r, ok := recs[id]; ok {
			return r
		}
		return LinkRecord{ID: string(id)}
	}
	out := make([]Link, 0, len(ls))
	for _, l := range ls {
		// A proposal between records an asserted link already joined has
		// nothing left to decide; it stays stored for when that link closes.
		if l.Kind == graph.SameAs && l.Status == graph.Proposed && cluster[l.A] != "" && cluster[l.A] == cluster[l.B] {
			continue
		}
		a, b := record(l.A), record(l.B)
		s := match.ScoreOf(l.Deposits, at, halfLife(model.Kind(a.Kind)))
		o := Link{
			ID: l.ID, Kind: string(l.Kind), Status: string(l.Status), Rule: l.Rule, A: a, B: b, ValidFrom: l.ValidFrom,
			Score:    LinkScore{Strength: s.Strength, Certainty: s.Certainty, Support: s.Support, Contrary: s.Contrary},
			Deposits: make([]LinkDeposit, len(l.Deposits)),
		}
		for i, d := range l.Deposits {
			o.Deposits[i] = LinkDeposit{At: d.At, Weight: d.Weight, Against: d.Against, ObservationID: d.ObservationID, Features: d.Features, Actor: d.Actor, Reason: d.Reason}
		}
		out = append(out, o)
	}
	slices.SortStableFunc(out, func(x, y Link) int { return -cmp.Compare(x.Score.Strength, y.Score.Strength) })
	limit := req.Limit
	if limit == 0 {
		limit = DefaultLinksLimit
	}
	return out[:min(limit, len(out))], nil
}
