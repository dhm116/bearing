package match

import (
	"testing"
	"time"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
)

func hierarchy(t *testing.T) *model.Registry {
	t.Helper()
	reg, err := model.Core().Extend([]model.KindDef{
		{Name: "k8s.Service", Parent: model.KindComponent},
		{Name: "systemd.Unit", Parent: model.KindComponent},
		{Name: "acme.Api", Parent: "k8s.Service"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestCandidatesPairRecordsAlongOneLineOfDescent(t *testing.T) {
	t.Parallel()
	recs := []graph.Record{
		rec("a-comp", "checkout", model.KindComponent),
		rec("b-k8s", "checkout", "k8s.Service"),
		rec("c-unit", "checkout", "systemd.Unit"),
		rec("d-api", "checkout", "acme.Api"),
		rec("e-team", "checkout", model.KindTeam),
	}
	var got []string
	for _, c := range Candidates(hierarchy(t), recs, nil, nil) {
		got = append(got, string(c.A.ID)+"+"+string(c.B.ID))
	}
	// Component with each of its descendants, k8s.Service with acme.Api;
	// never systemd.Unit with k8s.Service or acme.Api, nor anything with Team.
	want := []string{"a-comp+b-k8s", "a-comp+c-unit", "a-comp+d-api", "b-k8s+d-api"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestHalfLifeFallsBackToTheNearestAncestorWithASetting(t *testing.T) {
	t.Parallel()
	reg := hierarchy(t)
	cfg := Config{Registry: reg, HalfLife: map[model.Kind]time.Duration{
		model.KindComponent: 5 * 24 * time.Hour,
		"k8s.Service":       2 * 24 * time.Hour,
	}}
	tests := []struct {
		kind model.Kind
		want time.Duration
	}{
		{"acme.Api", 2 * 24 * time.Hour},
		{"k8s.Service", 2 * 24 * time.Hour},
		{"systemd.Unit", 5 * 24 * time.Hour},
		{model.KindComponent, 5 * 24 * time.Hour},
		{model.KindTeam, defaultHalfLife},
	}
	for _, tc := range tests {
		if got := cfg.HalfLifeOf(tc.kind); got != tc.want {
			t.Errorf("HalfLifeOf(%s) got %v, want %v", tc.kind, got, tc.want)
		}
	}
	if got := (Config{HalfLife: map[model.Kind]time.Duration{model.KindComponent: time.Hour}}).HalfLifeOf("k8s.Service"); got != defaultHalfLife {
		t.Errorf("without the registry a configured kind got %v, want the default", got)
	}
}
