package match

import (
	"context"
	"fmt"
	"slices"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

// Config tunes a Run. Zero fields take the defaults below.
type Config struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Matchers score candidate pairs; nil means []Matcher{Transparent{}}.
	Matchers []Matcher
	// ProposeAt is the score at which a pair becomes a proposal (default 0.6,
	// so an exact name alone qualifies and a near name alone does not).
	ProposeAt float64
	// Floor closes a proposal whose strength falls below it (default 0.1).
	Floor float64
	// ReviewAt puts a proposal at or above this strength in Result.Review
	// (default 0.3).
	ReviewAt float64
	// HalfLife is how fast evidence fades, by the kind of the linked records
	// (default 30 days).
	HalfLife map[model.Kind]time.Duration
	// Rules assert links from key patterns.
	Rules []KeyRule
}

const defaultHalfLife = 30 * 24 * time.Hour

func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Matchers == nil {
		c.Matchers = []Matcher{Transparent{}}
	}
	if c.ProposeAt == 0 {
		c.ProposeAt = 0.6
	}
	if c.Floor == 0 {
		c.Floor = 0.1
	}
	if c.ReviewAt == 0 {
		c.ReviewAt = 0.3
	}
	return c
}

func (c Config) halfLife(k model.Kind) time.Duration {
	if h, ok := c.HalfLife[k]; ok {
		return h
	}
	return defaultHalfLife
}

// Result is what one Run did.
type Result struct {
	// Asserted and Proposed count the links the rules and the matcher wrote
	// (new links and new deposits).
	Asserted, Proposed int
	// Closed counts proposals that decayed below the floor.
	Closed int
	// Review lists open proposals at or above ReviewAt, strongest first. They
	// are never promoted here.
	Review []Reviewable
}

// Reviewable is a proposal waiting for a person, with its working.
type Reviewable struct {
	Link  graph.Link
	Score Score
}

// Run applies the key rules, proposes links for candidate pairs, then closes
// proposals whose evidence has decayed. Running twice on unchanged evidence
// adds nothing the second time.
func Run(ctx context.Context, g IdentityGraph, cfg Config) (Result, error) {
	cfg = cfg.withDefaults()
	now := cfg.Now()
	for _, r := range cfg.Rules {
		if err := r.validate(); err != nil {
			return Result{}, fmt.Errorf("match: %w", err)
		}
	}
	recs, err := g.Records(ctx, "")
	if err != nil {
		return Result{}, fmt.Errorf("match: records: %w", err)
	}
	facts, err := g.Facts(ctx, graph.FactFilter{})
	if err != nil {
		return Result{}, fmt.Errorf("match: facts: %w", err)
	}
	links, err := g.Links(ctx, graph.LinkFilter{WithClosed: true})
	if err != nil {
		return Result{}, fmt.Errorf("match: links: %w", err)
	}

	// joined: pairs not worth a new link; lastAt: newest same_as deposit per pair.
	joined := map[pair]bool{}
	lastAt := map[pair]time.Time{}
	for _, l := range links {
		p := pairOf(l.A, l.B)
		open := l.ValidTo.IsZero()
		if open && (l.Kind == graph.DistinctFrom || l.Status == graph.Asserted) {
			joined[p] = true
		}
		for _, d := range l.Deposits {
			if l.Kind == graph.SameAs && d.At.After(lastAt[p]) {
				lastAt[p] = d.At
			}
		}
	}
	skip := func(a, b graph.EntityID) bool { return joined[pairOf(a, b)] }

	var res Result
	var ps []graph.Proposal
	seen := map[pair]bool{}
	for _, r := range cfg.Rules {
		for _, c := range r.apply(recs) {
			p := pairOf(c.A.ID, c.B.ID)
			if skip(c.A.ID, c.B.ID) || seen[p] {
				continue
			}
			seen[p] = true
			ps = append(ps, graph.Proposal{
				A: c.A.ID, B: c.B.ID, Kind: graph.SameAs, Status: graph.Asserted,
				Rule: "rule/" + r.Name, Deposit: graph.Deposit{At: now, Weight: 1},
			})
		}
	}
	res.Asserted = len(ps)

	ev := gather(facts)
	cands := Candidates(recs, ev.Members, func(a, b graph.EntityID) bool { return skip(a, b) || seen[pairOf(a, b)] })
	for _, m := range cfg.Matchers {
		js, err := m.Match(ctx, cands, ev)
		if err != nil {
			return Result{}, fmt.Errorf("match: %s: %w", m.Rule(), err)
		}
		for _, j := range js {
			c := j.Candidate
			at := laterOf(ev.Newest[c.A.Cluster], ev.Newest[c.B.Cluster])
			if j.Score < cfg.ProposeAt || at.IsZero() || !at.After(lastAt[pairOf(c.A.ID, c.B.ID)]) {
				continue
			}
			f := map[string]float64{"score": j.Score}
			for k, v := range j.Features {
				f[k] = v
			}
			ps = append(ps, graph.Proposal{
				A: c.A.ID, B: c.B.ID, Kind: graph.SameAs, Status: graph.Proposed,
				Rule: m.Rule(), Deposit: graph.Deposit{At: at, Weight: j.Score, Features: f},
			})
			res.Proposed++
		}
	}
	if len(ps) > 0 {
		if _, err := g.Link(ctx, ps); err != nil {
			return Result{}, fmt.Errorf("match: link: %w", err)
		}
	}

	open, err := g.Links(ctx, graph.LinkFilter{Kind: graph.SameAs, Status: graph.Proposed})
	if err != nil {
		return Result{}, fmt.Errorf("match: open proposals: %w", err)
	}
	kinds := map[graph.EntityID]model.Kind{}
	for _, r := range recs {
		kinds[r.ID] = r.Kind
	}
	for _, l := range open {
		s := ScoreOf(l.Deposits, now, cfg.halfLife(kinds[l.A]))
		switch {
		case s.Strength < cfg.Floor:
			if err := g.CloseLink(ctx, l.ID, now); err != nil {
				return Result{}, fmt.Errorf("match: close link %s: %w", l.ID, err)
			}
			res.Closed++
		case s.Strength >= cfg.ReviewAt:
			res.Review = append(res.Review, Reviewable{l, s})
		}
	}
	slices.SortStableFunc(res.Review, func(x, y Reviewable) int {
		switch {
		case x.Score.Strength > y.Score.Strength:
			return -1
		case x.Score.Strength < y.Score.Strength:
			return 1
		}
		return 0
	})
	return res, nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
