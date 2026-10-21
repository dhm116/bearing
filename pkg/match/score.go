package match

import (
	"math"
	"time"

	"bearing.example/pkg/graph"
)

// Score is a link's evidence at one moment (ADR 7).
type Score struct {
	// Support and Contrary are the deposits' weights, each decayed by age.
	Support, Contrary float64
	// Strength is Support/(Support+Contrary+1), between 0 and 1.
	Strength float64
	// Certainty is how much evidence there is, (Support+Contrary)/(Support+Contrary+1).
	Certainty float64
}

// ScoreOf decays each deposit as weight x 2^(-age/halfLife) and sums them by
// side. The prior of one keeps a link with no evidence at strength 0. A
// halfLife of zero or less means no decay, and a deposit from the future
// counts in full.
func ScoreOf(ds []graph.Deposit, now time.Time, halfLife time.Duration) Score {
	var s, c float64
	for _, d := range ds {
		w := d.Weight
		if age := now.Sub(d.At); halfLife > 0 && age > 0 {
			w *= math.Exp2(-float64(age) / float64(halfLife))
		}
		if d.Against {
			c += w
		} else {
			s += w
		}
	}
	return Score{Support: s, Contrary: c, Strength: s / (s + c + 1), Certainty: (s + c) / (s + c + 1)}
}
