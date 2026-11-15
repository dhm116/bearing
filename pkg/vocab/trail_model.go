package vocab

import (
	"context"
	"fmt"
	"slices"

	"bearing.example/pkg/systemone"
)

// Pager is an observed record a handle might page, with the name its source
// gives it.
type Pager struct{ ID, Key, Name string }

// Verdict is a System One model's answer to "is this record the one that
// handle names": the model, the question and p(true), kept as evidence.
type Verdict struct {
	Model, Question string
	P               float64
	Pager           string // key of the record asked about
}

// Ask puts the question for one broken pages step and one candidate record.
type Ask func(ctx context.Context, b Break, p Pager) (Verdict, error)

// JoinModel is Join, then asks the model about each broken pages step: for
// every observed pager, is it the one the unresolved handle names? The best
// answer at or above joinAt is taken as that step's object, and the trail
// joins again through it. A step the keys already proved is never asked
// about, since only an unobserved object breaks.
//
// The verdict is a feature, not a deposit: ADR 7 lets only independent
// source observations reinforce a link ("nothing Bearing inferred or
// answered can reinforce itself"), so p can only scale the step's strength
// (n/(n+1) of the handle's claims) down, never add to it. A trail's score
// is the product of its steps, so it carries the factor too.
//
// shortcut: asks every pager for every break, so n breaks cost n*m model
// calls; block on a normalized-name prefix first when a source has many.
func (t Trail) JoinModel(ctx context.Context, edges []Edge, pagers []Pager, ask Ask, joinAt float64) ([]Coverage, []Break, error) {
	cs, breaks := t.Join(edges)
	if ask == nil || len(breaks) == 0 {
		return cs, breaks, nil
	}
	verdicts := map[string]Verdict{} // fact ID of the handle's step -> best verdict
	resolved := map[string]Pager{}
	for _, b := range breaks {
		var best Verdict
		var bestP Pager
		for _, p := range pagers {
			v, err := ask(ctx, b, p)
			if err != nil {
				return nil, nil, fmt.Errorf("trail: ask about %s: %w", p.Key, err)
			}
			if v.P >= best.P {
				best, bestP = v, p
			}
		}
		if len(pagers) == 0 {
			continue
		}
		for _, f := range b.Claim.Facts {
			verdicts[f.Fact] = best
			if best.P >= joinAt {
				resolved[f.Fact] = bestP
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

// ModelAsk puts the pages question to a System One model: the message and
// service name are the state; the answer is p(true).
func ModelAsk(c *systemone.Client, h Handle) Ask {
	return func(ctx context.Context, b Break, p Pager) (Verdict, error) {
		handle, msg := h(b)
		q := fmt.Sprintf("Is the PagerDuty service named %q the one the handle %q pages?", p.Name, handle)
		a, err := c.Noul(ctx, map[string]string{"monitor_message": msg, "service_name": p.Name}, q)
		if err != nil {
			return Verdict{}, err
		}
		return Verdict{Model: a.Model, Question: q, P: a.P, Pager: p.Key}, nil
	}
}
