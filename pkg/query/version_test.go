package query

import "testing"

// The inputs are every shape the GitHub adapter emits (adapters/github
// manifests_test.go), and a few more ranges.
func TestLowerBoundReadsEveryManifestShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string // empty means unknown
	}{
		{"1.21", "1.21"},
		{"1.22.3", "1.22.3"},
		{"v20.1.0", "20.1.0"},
		{"20", "20"},
		{">=18", "18"},
		{" >=18 ", "18"},
		{">= 3.8", "3.8"},
		{"^20", "20"},
		{"~1.2.3", "1.2.3"},
		{"~=3.11", "3.11"},
		{">=3.10", "3.10"},
		{">=3.8,<4", "3.8"},
		{">=1.2 <2.0", "1.2"},
		{">=1.2 >=1.4", "1.4"},
		{"==3.11.*", "3.11"},
		{"20.x", "20"},
		{"18 || 20", "18"},
		{"^20 || ^18.2", "18.2"},
		{"3.12.1-rc1+build5", "3.12.1"},
		{"lts/hydrogen", ""},
		{"lts/*", ""},
		{"pypy3.10-7.3", ""},
		{"*", ""},
		{"x", ""},
		{"", ""},
		{"<5", ""},
		{"!=3.9", ""},
		{"18 || lts/iron", ""},
		{"1.22rc1", ""},
		{"system", ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			v, ok := lowerBound(tc.in)
			got := ""
			if ok {
				got = v.String()
			}
			if got != tc.want {
				t.Errorf("lowerBound(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestVersionCompareTreatsMissingPartsAsZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b version
		want int
	}{
		{version{1, 22}, version{1, 22, 0}, 0},
		{version{1, 21, 9}, version{1, 22}, -1},
		{version{20}, version{18, 9}, 1},
		{version{1, 9}, version{1, 10}, -1},
	} {
		if got := tc.a.compare(tc.b); got != tc.want {
			t.Errorf("%v compare %v = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
