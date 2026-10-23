package identityeval

import (
	"testing"
	"time"

	"bearing.example/pkg/match"
	"bearing.example/pkg/model"
)

// noDecay is a half-life far longer than any timeline here.
const noDecay = 100 * 365 * 24 * time.Hour

// runTimeline plays a scenario round by round: ingest, then match at the
// round's time, and return the open proposals after each round.
func runTimeline(t *testing.T, sc Scenario, halfLife time.Duration) []Snapshot {
	t.Helper()
	var now time.Time
	g := openGraph(t, func() time.Time { return now })
	cfg := match.Config{Now: func() time.Time { return now }, HalfLife: map[model.Kind]time.Duration{model.KindTeam: halfLife}}
	var cases []Case
	for _, p := range sc.Pairs {
		cases = append(cases, Case{Kind: model.KindTeam, A: p.A, B: p.B, Same: true})
	}
	var snaps []Snapshot
	for _, r := range sc.Rounds {
		now = r.At
		ingest(t, g, r.Observations)
		if _, err := match.Run(t.Context(), g, cfg); err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, Snapshot{At: r.At, Open: openProposals(t.Context(), t, g, cases)})
	}
	return snaps
}

// precisionOverTime is, per snapshot, the share of open proposals that are
// a still-true pair of the scenario; 1 with none open.
func precisionOverTime(sc Scenario, snaps []Snapshot) []float64 {
	out := make([]float64, len(snaps))
	for i, s := range snaps {
		right := 0
		for _, o := range s.Open {
			for _, p := range sc.Pairs {
				if NewPair(p.A, p.B) == NewPair(o.A, o.B) && (p.Until.IsZero() || s.At.Before(p.Until)) {
					right++
					break
				}
			}
		}
		out[i] = ratio(right, len(s.Open))
	}
	return out
}

func TestEvalDecayAgainstNone(t *testing.T) {
	t.Logf("%-18s %-8s %5s %10s %10s %6s %6s %7s", "scenario", "decay", "pairs", "mean lag", "max lag", "stuck", "flaps", "min.prec")
	for _, sc := range Scenarios(t0) {
		var minPrec [2]float64
		for i, v := range []struct {
			name string
			h    time.Duration
		}{{"default", 30 * 24 * time.Hour}, {"none", noDecay}} {
			snaps := runTimeline(t, sc, v.h)
			lag := StaleLag(sc.Pairs, snaps)
			flaps, opened := Flapping(snaps)
			minPrec[i] = 1
			for _, p := range precisionOverTime(sc, snaps) {
				minPrec[i] = min(minPrec[i], p)
			}
			t.Logf("%-18s %-8s %5d %10s %10s %6d %3d/%-2d %7.3f", sc.Name, v.name, lag.Pairs, lag.Mean, lag.Max, lag.Unresolved, flaps, opened, minPrec[i])
		}
		if minPrec[0] < minPrec[1] {
			t.Errorf("%s: decay lowered the worst precision to %.3f from %.3f without it", sc.Name, minPrec[0], minPrec[1])
		}
	}
}
