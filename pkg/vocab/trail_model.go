package vocab

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"bearing.example/pkg/systemone"
)

// Pager is an observed record a handle might page, with the name its source
// gives it.
type Pager struct{ ID, Key, Name string }

// NoPager is the Verdict.Pager when the model picked none of the records.
const NoPager = "none"

// Verdict is a System One model's answer to "which record does that handle
// name": the model, the question and the probability of the record it
// picked (or of NoPager), kept as evidence.
type Verdict struct {
	Model, Question string
	P               float64
	Pager           string // key of the record picked, or NoPager
}

// Ask puts one question for one broken pages step with every observed pager
// as an option.
type Ask func(ctx context.Context, b Break, pagers []Pager) (Verdict, error)

// JoinModel is Join, then asks the model once per broken pages step which
// observed pager the unresolved handle names. A pick at or above joinAt (not
// "none") is taken as that step's object, and the trail joins again through
// it; otherwise the break keeps the verdict. A step the keys already proved is never asked
// about, since only an unobserved object breaks.
//
// The verdict is a feature, not a deposit: ADR 7 lets only independent
// source observations reinforce a link ("nothing Bearing inferred or
// answered can reinforce itself"), so p can only scale the step's strength
// (n/(n+1) of the handle's claims) down, never add to it. A trail's score
// is the product of its steps, so it carries the factor too.
func (t Trail) JoinModel(ctx context.Context, edges []Edge, pagers []Pager, ask Ask, joinAt float64) ([]Coverage, []Break, error) {
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
			if best.Pager != NoPager && best.P >= joinAt {
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

// ModelAsk puts one multiple-choice question per broken step to a System
// One model: the message and handle are the state, each pager an option.
// Options are named by index, never by user text, and "none" is always one.
// The caller words the question in its tools' terms, as it reads the handle:
// the joiner knows no tool.
func ModelAsk(c *systemone.Client, h Handle, question string) Ask {
	return func(ctx context.Context, b Break, pagers []Pager) (Verdict, error) {
		handle, msg := h(b)
		q := question
		criteria := map[string]string{NoPager: "None of these"}
		for i, p := range pagers {
			criteria["s"+strconv.Itoa(i)] = p.Name
		}
		a, err := c.Choice(ctx, map[string]string{"monitor_message": msg, "handle": handle}, q, criteria)
		if err != nil {
			return Verdict{}, err
		}
		v := Verdict{Model: a.Model, Question: q, Pager: NoPager, P: a.Probabilities[a.Choice]}
		if a.Choice != NoPager {
			i, _ := strconv.Atoi(strings.TrimPrefix(a.Choice, "s"))
			v.Pager = pagers[i].Key
		}
		return v, nil
	}
}
