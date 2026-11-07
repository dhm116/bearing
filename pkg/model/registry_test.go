package model

import (
	"errors"
	"slices"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

func TestValidateObservationTakesConfiguredKindsAndTheirParentsRules(t *testing.T) {
	t.Parallel()
	o := NewObservation("adapter/test", time.Unix(0, 0), &modelv1alpha1.ObservationData{
		Entity: &modelv1alpha1.Entity{Kind: "acme.Api", Key: "test:svc/1"},
		Relations: []*modelv1alpha1.Relation{{
			Type: string(RelDependsOn), End: &modelv1alpha1.Relation_To{To: "test:svc/2"},
		}},
	})
	if err := ValidateObservation(extended(t), o); err != nil {
		t.Fatalf("got %v, want an acme.Api to depend_on like a Component", err)
	}
	var ve *ValidationError
	if err := ValidateObservation(Core(), o); !errors.As(err, &ve) || !ve.Has(codeNotDeclared) {
		t.Fatalf("got %v, want the core to refuse an unknown kind", err)
	}
}

// extended is Core plus k8s.Service under Component, acme.Api under
// k8s.Service, and systemd.Unit beside it.
func extended(t *testing.T) *Registry {
	t.Helper()
	reg, err := Core().Extend([]KindDef{
		{Name: "acme.Api", Parent: "k8s.Service"},
		{Name: "k8s.Service", Parent: KindComponent},
		{Name: "systemd.Unit", Parent: KindComponent},
		{Name: "vault.Policy"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRegistryExtendRefusals(t *testing.T) {
	t.Parallel()
	attr := Predicate{Name: "image", Type: tString, Cardinality: many, Conflict: noneC, Domain: []Kind{KindComponent}}
	tests := []struct {
		name  string
		kinds []KindDef
		preds []Predicate
		want  error
	}{
		{"lowercase kind", []KindDef{{Name: "k8s.service"}}, nil, ErrKindName},
		{"no namespace", []KindDef{{Name: "Service"}}, nil, ErrKindName},
		{"separator in kind", []KindDef{{Name: "k8s.Ser:vice"}}, nil, ErrKindName},
		{"reserved namespace", []KindDef{{Name: "bearing.Person"}}, nil, ErrKindName},
		{"core name again", []KindDef{{Name: "Person"}}, nil, ErrKindName},
		{"defined twice", []KindDef{{Name: "k8s.Service"}, {Name: "k8s.Service"}}, nil, ErrKindExists},
		{"missing parent", []KindDef{{Name: "k8s.Service", Parent: "Nothing"}}, nil, ErrUnknownKind},
		{"cycle", []KindDef{{Name: "a.X", Parent: "a.Y"}, {Name: "a.Y", Parent: "a.X"}}, nil, ErrKindCycle},
		{"own parent", []KindDef{{Name: "a.X", Parent: "a.X"}}, nil, ErrKindCycle},
		{"second shape", nil, []Predicate{{Name: "name", Type: tString, Cardinality: many, Conflict: noneC}}, ErrShapeConflict},
		{"attribute as relation", nil, []Predicate{{Name: "name", Relation: true, Type: tString, Cardinality: one, Conflict: noneC}}, ErrShapeConflict},
		{"dot in attribute", nil, []Predicate{{Name: "k8s.image", Type: tString, Cardinality: one}}, ErrPredicateName},
		{"same_as", nil, []Predicate{{Name: PredicateSameAs, Relation: true}}, ErrPredicateName},
		{"exists", nil, []Predicate{{Name: PredicateExists, Type: tBool, Cardinality: one}}, ErrPredicateName},
		{"unknown domain kind", nil, []Predicate{{Name: "image", Type: tString, Cardinality: one, Domain: []Kind{"k8s.Missing"}}}, ErrUnknownKind},
		{"core kind added to a core predicate's domain", nil, []Predicate{{Name: "name", Type: tString, Cardinality: one, Conflict: noneC, Domain: []Kind{KindComponent}}}, ErrCoreWiden},
		{"core kind added to a core relation's range", nil, []Predicate{{Name: string(RelDependsOn), Relation: true, Cardinality: many, Conflict: noneC, Range: []Kind{KindTeam}}}, ErrCoreWiden},
		{"new predicate without cardinality", nil, []Predicate{{Name: "image", Type: tString, Conflict: noneC}}, ErrPredicateShape},
		{"new predicate without conflict policy", nil, []Predicate{{Name: "image", Type: tString, Cardinality: one}}, ErrPredicateShape},
		{"new attribute without a type", nil, []Predicate{{Name: "image", Cardinality: one, Conflict: noneC}}, ErrPredicateShape},
		{"new relation without a type", []KindDef{{Name: "my-ns.Thing2"}}, []Predicate{{Name: "feeds", Relation: true, Cardinality: many, Conflict: noneC, Domain: []Kind{"my-ns.Thing2"}}}, nil},
		{"nil domain adds nothing to a core predicate", nil, []Predicate{{Name: "name", Type: tString, Cardinality: one, Conflict: noneC}}, nil},
		{"valid", []KindDef{{Name: "my-ns.Thing2"}}, []Predicate{attr}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core := Core()
			_, err := core.Extend(tc.kinds, tc.preds)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if core.Valid("my-ns.Thing2") || core.Valid("k8s.Service") {
				t.Fatal("Extend changed its receiver")
			}
		})
	}
}

func TestRegistryExtendMergesDomainAndRangeOfASharedPredicate(t *testing.T) {
	t.Parallel()
	reg, err := extended(t).Extend(nil, []Predicate{
		{Name: "name", Type: tString, Cardinality: one, Conflict: noneC, Domain: []Kind{"vault.Policy"}},
		{Name: string(RelDependsOn), Relation: true, Cardinality: many, Conflict: noneC, Domain: []Kind{"vault.Policy"}, Range: []Kind{"vault.Policy"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	name, _ := reg.Predicate("name")
	if !reg.InDomain(name, "vault.Policy") || !reg.InDomain(name, KindTeam) {
		t.Errorf("name domain %v, want Team and vault.Policy among it", name.Domain)
	}
	dep, _ := reg.Predicate(string(RelDependsOn))
	if !reg.InRange(dep, "vault.Policy") || !reg.InRange(dep, KindCloudResource) || reg.InRange(dep, KindTeam) {
		t.Errorf("depends_on range %v, want Component, CloudResource and vault.Policy", dep.Range)
	}
}

func TestRegistryHierarchy(t *testing.T) {
	t.Parallel()
	reg := extended(t)
	isA := []struct {
		k, anc Kind
		want   bool
	}{
		{"acme.Api", "acme.Api", true},
		{"acme.Api", "k8s.Service", true},
		{"acme.Api", KindComponent, true},
		{"k8s.Service", "acme.Api", false},
		{"k8s.Service", "systemd.Unit", false},
		{KindComponent, "k8s.Service", false},
		{"vault.Policy", KindComponent, false},
		{KindPerson, KindPerson, true},
	}
	for _, tc := range isA {
		if got := reg.IsA(tc.k, tc.anc); got != tc.want {
			t.Errorf("IsA(%s, %s) got %v, want %v", tc.k, tc.anc, got, tc.want)
		}
	}
	if got, want := reg.Descendants(KindComponent), []Kind{"Component", "acme.Api", "k8s.Service", "systemd.Unit"}; !slices.Equal(got, want) {
		t.Errorf("Descendants(Component) got %v, want %v", got, want)
	}
	if got, want := reg.Descendants("k8s.Service"), []Kind{"acme.Api", "k8s.Service"}; !slices.Equal(got, want) {
		t.Errorf("Descendants(k8s.Service) got %v, want %v", got, want)
	}
	if !reg.OneLine("acme.Api", KindComponent) || !reg.OneLine(KindComponent, "acme.Api") || reg.OneLine("k8s.Service", "systemd.Unit") {
		t.Error("OneLine must hold along a line of descent and fail across branches")
	}
	if got := reg.Deeper(KindComponent, "k8s.Service"); got != "k8s.Service" {
		t.Errorf("Deeper got %s, want k8s.Service", got)
	}
	if got := reg.Deeper("", KindTeam); got != KindTeam {
		t.Errorf("Deeper with an empty kind got %q, want Team", got)
	}
}

func TestRegistryInDomainAndRangeAcceptDescendants(t *testing.T) {
	t.Parallel()
	reg, err := extended(t).Extend(nil, []Predicate{{Name: "port", Type: tString, Cardinality: one, Conflict: noneC, Domain: []Kind{"k8s.Service"}}})
	if err != nil {
		t.Fatal(err)
	}
	dep, _ := reg.Predicate(string(RelDependsOn)) // Component to Component or CloudResource
	port, _ := reg.Predicate("port")              // an attribute of k8s.Service
	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{"descendant in domain", reg.InDomain(dep, "acme.Api"), true},
		{"descendant in range", reg.InRange(dep, "k8s.Service"), true},
		{"other branch below the same parent", reg.InDomain(dep, "systemd.Unit"), true},
		{"unrelated kind outside domain", reg.InDomain(dep, "vault.Policy"), false},
		{"grandchild in a child's domain", reg.InDomain(port, "acme.Api"), true},
		{"parent is not in a child's domain", reg.InDomain(port, KindComponent), false},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestRegistryExtendNilDomainAddsNothingAndEmptyStaysEmpty(t *testing.T) {
	t.Parallel()
	reg, err := Core().Extend(nil, []Predicate{{Name: "name", Type: tString, Cardinality: one, Conflict: noneC}})
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := reg.Predicate("name"); !reg.InDomain(name, KindTeam) || reg.InDomain(name, KindComponent) {
		t.Errorf("name domain %v, want it unchanged", name.Domain)
	}
	if got := union([]Kind{}, nil); got == nil || len(got) != 0 {
		t.Errorf("union of two empty lists got %#v, want an empty non-nil list", got)
	}
	if got := union(nil, []Kind{"a.B"}); got != nil {
		t.Errorf("union of any-kind with a kind got %v, want any kind (nil)", got)
	}
}

func TestNilRegistryReadsAsCore(t *testing.T) {
	t.Parallel()
	var reg *Registry
	if !reg.Valid(KindTeam) || reg.Valid("k8s.Service") || !reg.IsA(KindTeam, KindTeam) || reg.Parent(KindTeam) != "" {
		t.Error("a nil registry must answer as Core()")
	}
	if _, ok := reg.Predicate("name"); !ok {
		t.Error("a nil registry has no name predicate, want the core's")
	}
	if _, err := reg.Extend([]KindDef{{Name: "k8s.Service"}}, nil); err != nil {
		t.Errorf("Extend on nil got %v, want it to extend Core()", err)
	}
}
