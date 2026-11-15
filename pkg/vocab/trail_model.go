package vocab

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	jev "github.com/mattn/go-jev"
)

// Pager is an observed record a handle might page, with the name its source
// gives it.
type Pager struct{ ID, Key, Name string }

// NoPager is the Verdict.Pager when the model picked none of the records.
const NoPager = "none"

// Routes a Verdict gives a broken step.
const (
	RouteJoin   = "join"   // the handle names the record: the trail joins
	RouteReview = "review" // it may name it: the break stays for a person
	RouteNone   = "none"   // it names no record
)

// ScoreQuestion and ScoreLevels are the second question, asked about the
// record the first one picked. The levels were written before any score was
// seen; the middle one names the cases a person must check (TypeSafe's
// entity-alignment cookbook). A score rounds to a level: none, review, join.
var (
	ScoreQuestion = "How does the handle relate to this on-call service?"
	ScoreLevels   = []string{
		"The handle pages a different service, or no service.",
		"The handle may page this service, but it could be an old, renamed, retired, legacy or team-specific name for it, so a person should check.",
		"The handle pages this service.",
	}
)

// Verdict is a System One model's answer to "which record does that handle
// name, and how surely": the pick, its probability, the score of the pair and
// the route it gives, kept as evidence.
type Verdict struct {
	// Model is what the server says it served; some servers echo the name
	// they were sent, so Endpoint records which server answered.
	Model, Endpoint, Question string
	P                         float64 // the pick's probability
	Pager                     string  // key of the record picked, or NoPager
	Score                     float64 // 0 to 2 on ScoreLevels; unset for NoPager
	Route                     string  // RouteJoin, RouteReview or RouteNone
}

// Ask puts one question for one broken pages step with every observed pager
// as an option.
type Ask func(ctx context.Context, b Break, pagers []Pager) (Verdict, error)

// JoinModel is Join, then asks the model about each broken pages step: which
// observed pager the unresolved handle names, and how surely. A verdict that
// routes to join makes the pick that step's object, and the trail joins again
// through it; review or none leaves the break, with the verdict on it for a
// person. A step the keys already proved is never asked about, since only an
// unobserved object breaks.
//
// The verdict is a feature, not a deposit: ADR 7 lets only independent
// source observations reinforce a link ("nothing Bearing inferred or
// answered can reinforce itself"), so p can only scale the step's strength
// (n/(n+1) of the handle's claims) down, never add to it. A trail's score
// is the product of its steps, so it carries the factor too.
func (t Trail) JoinModel(ctx context.Context, edges []Edge, pagers []Pager, ask Ask) ([]Coverage, []Break, error) {
	cs, breaks := t.Join(edges)
	if ask == nil || len(breaks) == 0 {
		return cs, breaks, nil
	}
	verdicts := map[string]Verdict{} // fact ID of the handle's step -> best verdict
	resolved := map[string]Pager{}
	for _, b := range breaks {
		if len(pagers) == 0 {
			continue
		}
		best, err := ask(ctx, b, pagers)
		if err != nil {
			return nil, nil, fmt.Errorf("trail: ask about %s: %w", b.MonitorKey, err)
		}
		for _, f := range b.Claim.Facts {
			verdicts[f.Fact] = best
			if best.Route == RouteJoin {
				i := slices.IndexFunc(pagers, func(p Pager) bool { return p.Key == best.Pager })
				if i >= 0 {
					resolved[f.Fact] = pagers[i]
				}
			}
		}
	}
	if len(resolved) > 0 {
		edges = slices.Clone(edges)
		for i, e := range edges {
			if p, ok := resolved[e.Fact]; ok {
				edges[i].Object, edges[i].ObjectKey, edges[i].ObjectObserved = p.ID, p.Key, true
			}
		}
		cs, breaks = t.Join(edges)
	}
	for i := range cs {
		for j, cl := range cs[i].Page {
			if v, ok := verdicts[cl.Facts[0].Fact]; ok {
				cs[i].Page[j].Model = &v
				cs[i].Score *= v.P
				cs[i].Features["pages_model_p"] = v.P
			}
		}
	}
	for i := range breaks {
		if v, ok := verdicts[breaks[i].Claim.Facts[0].Fact]; ok {
			breaks[i].Verdict = &v
		}
	}
	return cs, breaks, nil
}

// Verdicts are the model answers behind the trail's steps, if any.
func (c Coverage) Verdicts() []Verdict {
	var out []Verdict
	for _, cl := range c.Page {
		if cl.Model != nil {
			out = append(out, *cl.Model)
		}
	}
	return out
}

// Handle says what a broken step's monitor notified: the handle text and
// the message it appeared in. The joiner knows no tool's terms, so the
// caller reads them from the monitor.
type Handle func(b Break) (handle, message string)

// ModelAsk puts two questions per broken step to a System One model. First,
// one multiple-choice question: the message and handle are the state, each
// pager an option (named by index, never by user text), and "none" is always
// one. Then, about the pick, a score on ScoreLevels, whose rounded level is
// the route.
// The caller words the question in its tools' terms, as it reads the handle:
// the joiner knows no tool.
func ModelAsk(c *jev.Client, h Handle, question string) Ask {
	return func(ctx context.Context, b Break, pagers []Pager) (Verdict, error) {
		handle, msg := h(b)
		opts := make(jev.Options, 0, len(pagers)+1)
		for i, p := range pagers {
			opts = append(opts, jev.Option{Name: "s" + strconv.Itoa(i), Desc: p.Name})
		}
		opts = append(opts, jev.Option{Name: NoPager, Desc: "None of these"})
		res, err := c.Evaluate(ctx, map[string]string{"monitor_message": msg, "handle": handle},
			map[string]jev.Question{"q": {Type: "choice", Instructions: question, Criteria: opts}})
		if err != nil {
			return Verdict{}, fmt.Errorf("ask %s: %w", c.URL(), err)
		}
		a := res.Answers["q"]
		if a == nil || a.Type != "choice" {
			return Verdict{}, fmt.Errorf("ask %s: no choice answer", c.URL())
		}
		v := Verdict{Model: res.Model, Endpoint: c.URL(), Question: question, Pager: NoPager, P: a.Probabilities[a.Choice]}
		if a.Choice != NoPager {
			i, err := strconv.Atoi(strings.TrimPrefix(a.Choice, "s"))
			if err != nil || i < 0 || i >= len(pagers) || !strings.HasPrefix(a.Choice, "s") {
				return Verdict{}, fmt.Errorf("ask %s: picked %q, which was not offered", c.URL(), a.Choice)
			}
			v.Pager = pagers[i].Key
			res, err := c.Evaluate(ctx, map[string]string{"monitor_message": msg, "handle": handle, "service": pagers[i].Name},
				map[string]jev.Question{"q": {Type: "score", Instructions: ScoreQuestion, Criteria: ScoreLevels}})
			if err != nil {
				return Verdict{}, fmt.Errorf("ask %s: %w", c.URL(), err)
			}
			sa := res.Answers["q"]
			if sa == nil || sa.Type != "score" {
				return Verdict{}, fmt.Errorf("ask %s: no score answer", c.URL())
			}
			v.Score = sa.Score
		}
		v.Route = route(v)
		return v, nil
	}
}

// route rounds a verdict's score to a level: under 0.5 none, under 1.5
// review, else join. A pick of none is none.
func route(v Verdict) string {
	switch {
	case v.Pager == NoPager || v.Score < 0.5:
		return RouteNone
	case v.Score < 1.5:
		return RouteReview
	default:
		return RouteJoin
	}
}
