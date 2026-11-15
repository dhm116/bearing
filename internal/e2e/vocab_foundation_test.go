package e2e

import (
	"strings"
	"testing"

	"bearing.example/adapters/kubernetes"
	"bearing.example/pkg/vocab"
)

// TestVocabFoundationAcme is ADR 13's first step on Acme: the core module and
// the adapter's io.k8s module load into the registry, payments-api is stored
// under the adapter's own term with only io.k8s attributes and relations,
// and "which team owns payments-api" answers from the operator's r-team rule,
// with the label observation and the rule as evidence. Nothing the adapter
// sent is a core term.
func TestVocabFoundationAcme(t *testing.T) {
	t.Parallel()
	v := newVocabWorld(t, nil)
	v.Settings = []vocab.Mapping{teamRule(1, teamKey)}
	v.boot()
	for _, o := range v.sync() {
		d := o.GetData()
		if k := d.GetEntity().GetKind(); !strings.HasPrefix(k, "io.k8s:") {
			t.Errorf("%s: kind %s is not an io.k8s term", o.GetId(), k)
		}
		for a := range d.GetEntity().GetAttributes() {
			if !strings.HasPrefix(a, "io.k8s:") {
				t.Errorf("%s: attribute %s is not an io.k8s term", o.GetId(), a)
			}
		}
		for _, r := range d.GetRelations() {
			if !strings.HasPrefix(r.GetType(), "io.k8s:") {
				t.Errorf("%s: relation %s is not an io.k8s term", o.GetId(), r.GetType())
			}
		}
	}

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
	if r := o.Owners[0].Rules; len(r) != 1 || r[0] != "r-team@1" {
		t.Errorf("rules = %v, want the answer to name r-team@1", r)
	}
	rules, err := v.store.RuleCounts(t.Context())
	if err != nil || rules[""] == 0 || rules["r-team@1"] == 0 || len(rules) != 2 {
		t.Errorf("fact_source rules = %v, %v; want stated facts ('') and r-team@1's", rules, err)
	}

	if got, err := v.store.ModuleVersions(t.Context(), "kubernetes-acme"); err != nil || len(got) != 1 || got[0] != "io.k8s@"+kubernetes.Version {
		t.Errorf("module versions = %v, %v; want io.k8s@%s on every observation", got, err, kubernetes.Version)
	}

	terms, err := v.store.Terms(t.Context(), vocab.CoreNamespace)
	if err != nil || len(terms) != len(vocab.Core().Terms) || terms[0].Source.VerifiedBy != "build" {
		t.Errorf("core terms: %d rows, %v; want %d from the build", len(terms), err, len(vocab.Core().Terms))
	}
	k8s, err := v.store.Terms(t.Context(), "io.k8s")
	if want := len(kubernetes.Module().Terms); err != nil || len(k8s) != want {
		t.Errorf("io.k8s terms = %+v, %v; want the module's %d", k8s, err, want)
	}
}
