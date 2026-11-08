package adapter

import (
	"slices"
	"testing"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
)

func TestDropInactiveKeepsOnlyObservationsOfEnabledKinds(t *testing.T) {
	t.Parallel()
	of := func(id, kind string) *eventv1alpha1.Observation {
		return &eventv1alpha1.Observation{Id: id, Data: &modelv1alpha1.ObservationData{Entity: &modelv1alpha1.Entity{Kind: kind}}}
	}
	obs := []*eventv1alpha1.Observation{
		of("repo", "Repository"), of("svc", "k8s.Service"), of("nokind", ""), of("team", "Team"), of("alien", "vault.Group"),
	}
	tests := []struct {
		name    string
		enabled []model.Kind
		want    []string
	}{
		{"no filter when none are enabled", nil, []string{"repo", "svc", "nokind", "team", "alien"}},
		{"one kind", []model.Kind{"Repository"}, []string{"repo", "nokind"}},
		{"several kinds, an unknown one dropped", []model.Kind{"Repository", "Team"}, []string{"repo", "nokind", "team"}},
		{"a kind the registry lacks, when enabled", []model.Kind{"k8s.Service"}, []string{"svc", "nokind"}},
		{"a kind nothing uses", []model.Kind{"Person"}, []string{"nokind"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, o := range DropInactive(t.Context(), model.Core(), "src", obs, tt.enabled) {
				got = append(got, o.GetId())
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
