package match

import "testing"

func TestJaroWinklerKnownValues(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"martha", "marhta", 0.9611},
		{"dixon", "dicksonx", 0.8133},
		{"dwayne", "duane", 0.84},
		{"payments", "payments", 1},
		{"payments", "payroll", 0.6012},
		{"abc", "xyz", 0},
		{"", "abc", 0},
	}
	for _, tc := range tests {
		t.Run(tc.a+"/"+tc.b, func(t *testing.T) {
			got := jaroWinkler(tc.a, tc.b)
			if d := got - tc.want; d > 0.001 || d < -0.001 {
				t.Fatalf("got %.4f, want %.4f", got, tc.want)
			}
		})
	}
}

func TestNormalizeFoldsSeparators(t *testing.T) {
	tests := map[string]string{
		"Payments Team":  "payments team",
		"payments-team":  "payments team",
		"payments__team": "payments team",
		" SRE ":          "sre",
		"":               "",
	}
	for in, want := range tests {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJaccardOfSets(t *testing.T) {
	set := func(ks ...string) map[string]bool {
		m := map[string]bool{}
		for _, k := range ks {
			m[k] = true
		}
		return m
	}
	tests := []struct {
		name string
		a, b map[string]bool
		want float64
	}{
		{"both empty", nil, nil, 0},
		{"disjoint", set("a"), set("b"), 0},
		{"half", set("a", "b"), set("b", "c"), 1.0 / 3},
		{"equal", set("a", "b"), set("a", "b"), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jaccard(tc.a, tc.b); !near(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
