package identityeval

import (
	"slices"
	"time"
)

// PrecisionRecall scores proposed same_as pairs against the labels. A
// proposal is right only for a pair labelled same; a pair no case names is
// different, so proposing it is wrong. With no proposals precision is 1; with
// no same cases recall is 1.
func PrecisionRecall(cases []Case, proposed []Pair) (precision, recall float64) {
	same := map[Pair]bool{}
	for _, c := range cases {
		if c.Same {
			same[c.Pair()] = true
		}
	}
	seen := map[Pair]bool{}
	tp := 0
	for _, p := range proposed {
		p = NewPair(p.A, p.B)
		if seen[p] {
			continue
		}
		seen[p] = true
		if same[p] {
			tp++
		}
	}
	return ratio(tp, len(seen)), ratio(tp, len(same))
}

// BlockingRecall is the share of pairs labelled same that candidates
// contains: a pair blocking misses can never be proposed.
func BlockingRecall(cases []Case, candidates []Pair) float64 {
	have := map[Pair]bool{}
	for _, p := range candidates {
		have[NewPair(p.A, p.B)] = true
	}
	found, total := 0, 0
	for _, c := range cases {
		if c.Same {
			total++
			if have[c.Pair()] {
				found++
			}
		}
	}
	return ratio(found, total)
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 1
	}
	return float64(n) / float64(d)
}

// Snapshot is the proposals open after one matcher run at At.
type Snapshot struct {
	At   time.Time
	Open []Pair
}

func (s Snapshot) has(p Pair) bool {
	return slices.Contains(s.Open, p) || slices.Contains(s.Open, Pair{p.B, p.A})
}

// Lag summarizes how long stale proposals outlived their evidence.
type Lag struct {
	// Pairs is how many pairs had a truth change and were open when it
	// came.
	Pairs int
	// Mean and Max are over the pairs that closed, from the change to the
	// first snapshot without the proposal.
	Mean, Max time.Duration
	// Unresolved pairs were still open in the last snapshot; their lag is
	// not in Mean and Max.
	Unresolved int
}

// StaleLag measures, for each pair whose truth changed (a Scenario's
// Pairs with an Until time), how long its proposal stayed open after the
// change. Snapshots must be in time order; the resolution is the gap
// between them. A pair not open at the change counts for nothing.
func StaleLag(pairs []TimedPair, snaps []Snapshot) Lag {
	var l Lag
	var total time.Duration
	closed := 0
	for _, tp := range pairs {
		if tp.Until.IsZero() {
			continue
		}
		p := NewPair(tp.A, tp.B)
		idx := slices.IndexFunc(snaps, func(s Snapshot) bool { return !s.At.Before(tp.Until) })
		if idx < 0 || !snaps[idx].has(p) {
			continue
		}
		l.Pairs++
		end := slices.IndexFunc(snaps[idx:], func(s Snapshot) bool { return !s.has(p) })
		if end < 0 {
			l.Unresolved++
			continue
		}
		d := snaps[idx+end].At.Sub(tp.Until)
		total += d
		closed++
		l.Max = max(l.Max, d)
	}
	if closed > 0 {
		l.Mean = total / time.Duration(closed)
	}
	return l
}

// Flapping counts proposals that reopened after closing, across snapshots:
// the pair crossed the floor and came back. Opened is how many distinct
// pairs were ever open, the denominator for a rate.
func Flapping(snaps []Snapshot) (reopens, opened int) {
	was := map[Pair]bool{}  // open in the previous snapshot
	ever := map[Pair]bool{} // open in any earlier snapshot
	for _, s := range snaps {
		now := map[Pair]bool{}
		for _, p := range s.Open {
			p = NewPair(p.A, p.B)
			now[p] = true
			if ever[p] && !was[p] {
				reopens++
			}
			ever[p] = true
		}
		was = now
	}
	return reopens, len(ever)
}
