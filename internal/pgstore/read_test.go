package pgstore

import (
	"slices"
	"testing"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

func TestClaimScopeFindsFactsPointingAtASubjectBySubject(t *testing.T) {
	obj := &modelv1alpha1.FactObject{SubjectId: "sub-1"}
	for _, c := range []struct {
		name string
		f    contracts.FactFilter
		want []string
	}{
		{"an object alone", contracts.FactFilter{Predicate: "changed_by", Object: obj}, []string{"sub-1"}},
		{"a subject and an object", contracts.FactFilter{SubjectID: "sub-2", Object: obj}, []string{"sub-2"}},
		{"a key and an object", contracts.FactFilter{Key: "k", Object: obj}, nil},
		{"a predicate alone", contracts.FactFilter{Predicate: "changed_by"}, nil},
		{"an object that names no subject", contracts.FactFilter{Predicate: "changed_by", Object: &modelv1alpha1.FactObject{}}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := claimScope(c.f).ClaimSubjects
			if !slices.Equal(got, c.want) {
				t.Fatalf("got claim subjects %v, want %v", got, c.want)
			}
		})
	}
}
