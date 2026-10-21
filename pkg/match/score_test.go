package match

import (
	"math"
	"testing"
	"time"

	"bearing.example/pkg/graph"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScoreOfDecaysDeposits(t *testing.T) {
	tests := []struct {
		name              string
		ds                []graph.Deposit
		now               time.Time
		half              time.Duration
		support, contrary float64
		strength, certain float64
	}{
		{"no deposits", nil, t0, 30 * day, 0, 0, 0, 0},
		{"fresh", []graph.Deposit{{At: t0, Weight: 1}}, t0, 30 * day, 1, 0, 0.5, 0.5},
		{"one half-life", []graph.Deposit{{At: t0, Weight: 1}}, t0.Add(30 * day), 30 * day, 0.5, 0, 1.0 / 3, 1.0 / 3},
		{"two half-lives", []graph.Deposit{{At: t0, Weight: 2}}, t0.Add(60 * day), 30 * day, 0.5, 0, 1.0 / 3, 1.0 / 3},
		{"contrary counts against", []graph.Deposit{{At: t0, Weight: 1}, {At: t0, Weight: 1, Against: true}}, t0, 30 * day, 1, 1, 1.0 / 3, 2.0 / 3},
		{"old contrary fades", []graph.Deposit{{At: t0, Weight: 1, Against: true}, {At: t0.Add(30 * day), Weight: 1}}, t0.Add(30 * day), 30 * day, 1, 0.5, 1 / 2.5, 1.5 / 2.5},
		{"future deposit counts in full", []graph.Deposit{{At: t0.Add(day), Weight: 1}}, t0, 30 * day, 1, 0, 0.5, 0.5},
		{"zero half-life never decays", []graph.Deposit{{At: t0, Weight: 1}}, t0.Add(900 * day), 0, 1, 0, 0.5, 0.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreOf(tc.ds, tc.now, tc.half)
			want := Score{tc.support, tc.contrary, tc.strength, tc.certain}
			if !near(got.Support, want.Support) || !near(got.Contrary, want.Contrary) ||
				!near(got.Strength, want.Strength) || !near(got.Certainty, want.Certainty) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}
