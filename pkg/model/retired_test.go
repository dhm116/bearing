package model

import (
	"testing"
	"time"
)

func TestWithRetiredKeepsRetiredKindsValidAndMergesPredicateLists(t *testing.T) {
	t.Parallel()
	gone := time.Unix(50, 0).UTC()
	base, err := Core().Extend([]KindDef{{Name: "io.k8s:Service", Parent: KindComponent}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owned, _ := base.Predicate("owned_by")
	owned.Domain = []Kind{KindComponent, "com.acme:Api"}
	r := base.WithRetired(
		[]KindDef{{Name: "com.acme:Api", Parent: "io.k8s:Service", Retired: gone}, {Name: "io.k8s:Service", Retired: gone}},
		[]Predicate{owned, {Name: "old_attr", Cardinality: owned.Cardinality}},
	)
	if got, ok := r.Retired("com.acme:Api"); !ok || !got.Equal(gone) {
		t.Fatalf("got Retired(com.acme:Api) = %v, %v, want %v, true", got, ok, gone)
	}
	if _, ok := r.Retired("io.k8s:Service"); ok {
		t.Fatal("got an active kind retired by WithRetired")
	}
	if !r.Valid("com.acme:Api") || !r.IsA("com.acme:Api", KindComponent) {
		t.Fatal("got a retired kind that is invalid or lost its parents")
	}
	if p, _ := r.Predicate("owned_by"); len(p.Domain) != 2 {
		t.Fatalf("got domain %v, want the passed one", p.Domain)
	}
	if _, ok := r.Predicate("old_attr"); !ok {
		t.Fatal("got no old_attr predicate")
	}
	if _, ok := base.Retired("com.acme:Api"); ok || base.Valid("com.acme:Api") {
		t.Fatal("WithRetired changed its receiver")
	}
	if n := len(r.Predicates()); n != len(base.Predicates())+1 {
		t.Fatalf("got %d predicates, want %d", n, len(base.Predicates())+1)
	}
}
