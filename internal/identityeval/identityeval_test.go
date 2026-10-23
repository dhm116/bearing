package identityeval

import (
	"reflect"
	"testing"
	"time"

	"bearing.example/pkg/model"
)

var t0 = time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

func TestGenerateIsDeterministic(t *testing.T) {
	t.Parallel()
	a, b := Generate(7, t0), Generate(7, t0)
	if !reflect.DeepEqual(a.Cases, b.Cases) || len(a.Observations) != len(b.Observations) {
		t.Fatal("same seed gave different sets")
	}
	for i := range a.Observations {
		if a.Observations[i].GetId() != b.Observations[i].GetId() {
			t.Fatalf("observation %d: got %s, want %s", i, b.Observations[i].GetId(), a.Observations[i].GetId())
		}
	}
	if c := Generate(8, t0); reflect.DeepEqual(a.Cases, c.Cases) {
		t.Error("different seeds gave the same cases")
	}
}

func TestGenerateLabelsAreBalancedAndKeysResolve(t *testing.T) {
	t.Parallel()
	s := Generate(1, t0)
	same, seen, groups := 0, map[Pair]bool{}, map[string]bool{}
	for _, c := range s.Cases {
		if c.Same {
			same++
		}
		if seen[c.Pair()] {
			t.Errorf("pair %v labelled twice", c.Pair())
		}
		seen[c.Pair()] = true
		groups[c.Group] = true
	}
	n := len(s.Cases)
	if n < 150 || same < n/3 || same > n*2/3 {
		t.Errorf("got %d cases, %d same; want at least 150 with between a third and two thirds same", n, same)
	}
	for _, g := range []string{"acme", "convention", "rename", "rename-collision", "near-miss", "unrelated", "person-same-email", "person-alias", "person-same-name", "person-different"} {
		if !groups[g] {
			t.Errorf("no cases in group %s", g)
		}
	}
	kinds := map[model.Key]model.Kind{}
	for _, o := range s.Observations {
		if err := model.ValidateObservation(o); err != nil {
			t.Fatalf("%s: %v", o.GetId(), err)
		}
		kinds[model.Key(o.GetData().GetEntity().GetKey())] = model.Kind(o.GetData().GetEntity().GetKind())
	}
	for _, c := range s.Cases {
		if kinds[c.A] != c.Kind || kinds[c.B] != c.Kind {
			t.Errorf("case %v: kinds %q and %q, want %q", c.Pair(), kinds[c.A], kinds[c.B], c.Kind)
		}
	}
}

func TestScenariosValidateAndStopEvidence(t *testing.T) {
	t.Parallel()
	for _, sc := range Scenarios(t0) {
		if len(sc.Rounds) != Rounds {
			t.Errorf("%s: got %d rounds, want %d", sc.Name, len(sc.Rounds), Rounds)
		}
		sources := func(r Round, key model.Key) int {
			n := 0
			for _, o := range r.Observations {
				if o.GetData().GetEntity().GetKey() == string(key) {
					n++
				}
			}
			return n
		}
		for _, r := range sc.Rounds {
			for _, o := range r.Observations {
				if err := model.ValidateObservation(o); err != nil {
					t.Fatalf("%s: %s: %v", sc.Name, o.GetId(), err)
				}
			}
		}
		for _, p := range sc.Pairs {
			last := sc.Rounds[Rounds-1]
			switch sc.Name {
			case "deleted-github":
				if n := sources(last, p.A); n != 0 || sources(last, p.B) != 1 || p.Until.IsZero() {
					t.Errorf("%s: github team still observed or truth never changes", sc.Name)
				}
			case "deleted-directory":
				if n := sources(last, p.B); n != 0 || sources(last, p.A) != 1 || p.Until.IsZero() {
					t.Errorf("%s: directory team still observed or truth never changes", sc.Name)
				}
			default:
				if sources(last, p.A) != 1 || sources(last, p.B) != 1 || !p.Until.IsZero() {
					t.Errorf("%s: want both teams observed to the end and truth unchanged", sc.Name)
				}
			}
		}
	}
}

func TestPrecisionRecall(t *testing.T) {
	t.Parallel()
	cases := []Case{
		{A: "x:k/a", B: "y:k/a", Same: true},
		{A: "x:k/b", B: "y:k/b", Same: true},
		{A: "x:k/c", B: "y:k/c", Same: true},
		{A: "x:k/d", B: "y:k/d", Same: false},
	}
	tests := []struct {
		name         string
		proposed     []Pair
		wantP, wantR float64
	}{
		{"none", nil, 1, 0},
		{"all right", []Pair{{"y:k/a", "x:k/a"}, {"x:k/b", "y:k/b"}}, 1, 2.0 / 3},
		{"a wrong label and an unlabelled pair", []Pair{{"x:k/a", "y:k/a"}, {"x:k/d", "y:k/d"}, {"x:k/a", "y:k/b"}}, 1.0 / 3, 1.0 / 3},
		{"duplicates count once", []Pair{{"x:k/a", "y:k/a"}, {"y:k/a", "x:k/a"}}, 1, 1.0 / 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, r := PrecisionRecall(cases, tt.proposed)
			if p != tt.wantP || r != tt.wantR {
				t.Errorf("got precision %v recall %v, want %v and %v", p, r, tt.wantP, tt.wantR)
			}
		})
	}
	if got := CandidateRecall(cases, []Pair{{"y:k/a", "x:k/a"}, {"x:k/d", "y:k/d"}}); got != 1.0/3 {
		t.Errorf("candidate recall: got %v, want 1/3", got)
	}
	if got := CandidateRecall(nil, nil); got != 1 {
		t.Errorf("candidate recall of no cases: got %v, want 1", got)
	}
}

func TestStaleLagAndFlapping(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	at := func(d int) time.Time { return t0.Add(time.Duration(d) * day) }
	p1, p2, p3 := Pair{"a:k/1", "b:k/1"}, Pair{"a:k/2", "b:k/2"}, Pair{"a:k/3", "b:k/3"}
	snaps := []Snapshot{
		{at(0), []Pair{p1, p2, p3}},
		{at(7), []Pair{p1, p2, p3}},
		{at(14), []Pair{p1, p2}},           // p3 closes
		{at(21), []Pair{p1, {p2.B, p2.A}}}, // p2 still open, written the other way
		{at(28), []Pair{p1}},               // p2 closes
		{at(35), []Pair{p1, p3}},           // p3 comes back
	}
	pairs := []TimedPair{
		{A: p1.A, B: p1.B},                // truth never changes
		{A: p2.A, B: p2.B, Until: at(8)},  // first snapshot after the change is day 14, closes at 28: 20 days
		{A: p3.A, B: p3.B, Until: at(10)}, // already closed at day 14: no stale proposal
	}
	got := StaleLag(pairs, snaps)
	want := Lag{Pairs: 1, Mean: 20 * day, Max: 20 * day}
	// p3 is closed in the first snapshot at or after day 10, so it counts for nothing.
	if got != want {
		t.Errorf("stale lag: got %+v, want %+v", got, want)
	}
	if got := StaleLag([]TimedPair{{A: p1.A, B: p1.B, Until: at(8)}}, snaps); got.Pairs != 1 || got.Unresolved != 1 {
		t.Errorf("never closed: got %+v, want one unresolved pair", got)
	}
	if re, opened := Flapping(snaps); re != 1 || opened != 3 {
		t.Errorf("flapping: got %d reopens of %d pairs, want 1 of 3", re, opened)
	}
}
