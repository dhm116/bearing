package e2e

import (
	"testing"

	"bearing.example/pkg/vocab"
)

// TestVocabFoundationAcme is ADR 13's first step on Acme: the core module and
// the adapter's io.k8s module load into today's registry, payments-api is
// stored under the adapter's own term, and "which team owns payments-api"
// answers as before, with the label observation as evidence and no rule
// behind it.
func TestVocabFoundationAcme(t *testing.T) {
	t.Parallel()
	v := newVocabWorld(t, nil)
	v.boot()
	v.sync()

	e, err := v.q.Get(t.Context(), paymentsWorkload, v.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "io.k8s:Deployment" {
		t.Errorf("payments-api kind = %q, want io.k8s:Deployment", e.Kind)
	}
	o, err := v.q.Owners(t.Context(), paymentsWorkload, v.clock.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Owners) != 1 || o.Owners[0].Object == nil || o.Owners[0].Object.Name != "payments" {
		t.Fatalf("owners = %+v, want the payments team", o.Owners)
	}
	src := o.Owners[0].Sources
	if len(src) != 1 || src[0].Source != "kubernetes-acme" || src[0].ObservationID == "" {
		t.Errorf("evidence = %+v, want one kubernetes-acme observation", src)
	}
	rules, err := v.store.RuleCounts(t.Context())
	if err != nil || len(rules) != 1 || rules[""] == 0 {
		t.Errorf("fact_source rules = %v, %v; want only '' (no rule made these facts)", rules, err)
	}

	terms, err := v.store.Terms(t.Context(), vocab.CoreNamespace)
	if err != nil || len(terms) != len(vocab.Core().Terms) || terms[0].Source.VerifiedBy != "build" {
		t.Errorf("core terms: %d rows, %v; want %d from the build", len(terms), err, len(vocab.Core().Terms))
	}
	k8s, err := v.store.Terms(t.Context(), "io.k8s")
	if err != nil || len(k8s) != 6 {
		t.Errorf("io.k8s terms = %+v, %v; want the module's 6", k8s, err)
	}
}
