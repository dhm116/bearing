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

// defaultHalfLife is match's default, the one the weekly scenarios use.
const defaultHalfLife = 30 * 24 * time.Hour

// TestEvalDecayAgainstNone plays each weekly scenario with the default
// half-life and with none. With decay, a proposal whose record was deleted
// must close before the timeline ends.
func TestEvalDecayAgainstNone(t *testing.T) {
	t.Parallel()
	for _, sc := range Scenarios(t0) {
		for _, v := range []struct {
			name string
			h    time.Duration
		}{{"default", defaultHalfLife}, {"none", noDecay}} {
			t.Run(sc.Name+"/"+v.name, func(t *testing.T) {
				t.Parallel()
				snaps := runTimeline(t, sc, v.h)
				lag := StaleLag(sc.Pairs, snaps)
				flaps, opened := Flapping(snaps)
				minPrec := 1.0
				for _, p := range precisionOverTime(sc, snaps) {
					minPrec = min(minPrec, p)
				}
				t.Logf("pairs ended %d, mean lag %s, max lag %s, stuck %d, flaps %d/%d, worst precision %.3f",
					lag.Pairs, lag.Mean, lag.Max, lag.Unresolved, flaps, opened, minPrec)
				if v.h == defaultHalfLife && lag.Unresolved != 0 {
					t.Errorf("%d stale proposals still open at the end, want 0", lag.Unresolved)
				}
			})
		}
	}
}

// TestEvalHourlyStaleProposalClosesWithinFourHalfLives syncs both sides every
// hour, so a matcher that reinforced unchanged evidence, or dated it by the
// side still observed, would keep the proposal alive long after GitHub
// deleted its team.
func TestEvalHourlyStaleProposalClosesWithinFourHalfLives(t *testing.T) {
	t.Parallel()
	const halfLife = 7 * 24 * time.Hour // a week, so the 40 days after the deletion hold several
	sc := Hourly(t0)
	snaps := runTimeline(t, sc, halfLife)
	lag := StaleLag(sc.Pairs, snaps)
	flaps, opened := Flapping(snaps)
	t.Logf("pairs ended %d, lag %s, stuck %d, flaps %d/%d", lag.Pairs, lag.Max, lag.Unresolved, flaps, opened)
	if lag.Pairs != 1 || lag.Unresolved != 0 || lag.Max > 4*halfLife {
		t.Fatalf("got %+v, want the one stale proposal closed within %s", lag, 4*halfLife)
	}
}
