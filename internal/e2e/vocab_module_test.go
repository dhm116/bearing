package e2e

import (
	"testing"
	"time"

	"bearing.example/pkg/vocab"
)

// describedModule is the adapter's module as the server gets it, from
// Describe, checked the way the server checks an adapter's module.
func describedModule(v *vocabWorld) vocab.Module {
	v.t.Helper()
	d, err := v.a.Describe(v.t.Context())
	if err != nil || d.Vocabulary == nil {
		v.t.Fatalf("describe: %v, vocabulary %v", err, d.Vocabulary)
	}
	if err := d.Vocabulary.CheckAdapter(); err != nil {
		v.t.Fatal(err)
	}
	return *d.Vocabulary
}

func (v *vocabWorld) paymentsIsComponent() bool {
	v.t.Helper()
	ms, err := v.q.Find(v.t.Context(), "payments-api", "Component", 0)
	if err != nil {
		v.t.Fatal(err)
	}
	return len(ms) > 0
}

// TestVocabModuleAcceptBroaderAfterBoot is ADR 12's "extends later" blocker
// gone: the adapter ships its module in Describe, its suggestion arrives
// proposed, and accepting it after first boot makes Deployments Components
// without a restart of the adapter or a change to the core.
func TestVocabModuleAcceptBroaderAfterBoot(t *testing.T) {
	t.Parallel()
	v := newVocabWorld(t, nil)
	v.Modules = []vocab.Module{describedModule(v)}
	v.boot()
	v.sync()
	ctx := t.Context()

	if v.paymentsIsComponent() {
		t.Fatal("payments-api is a Component before the mapping is accepted")
	}
	ms, err := v.store.Mappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const id = "k8s-deployment-is-component"
	var open []vocab.Mapping
	for _, m := range ms {
		if m.ID == id {
			open = append(open, m)
		}
	}
	if len(open) != 1 || open[0].Status != vocab.Proposed || open[0].Subject != "io.k8s:Deployment" || open[0].Object != vocab.CoreNamespace+":Component" {
		t.Fatalf("mapping %s rows = %+v, want one proposed with full names", id, open)
	}

	v.clock.Advance(time.Hour)
	if err := v.store.DecideMapping(ctx, id, vocab.Asserted, "ops@acme", "Each Deployment is one service at Acme", v.clock.Now()); err != nil {
		t.Fatal(err)
	}
	v.clock.Advance(time.Minute)
	v.boot() // same store: the module is unchanged, so no refusal
	v.sync()

	if !v.paymentsIsComponent() {
		t.Fatal("payments-api is not a Component after the mapping is accepted")
	}
	o, err := v.q.Owners(ctx, paymentsWorkload, v.clock.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Owners) != 1 || o.Owners[0].Object == nil || o.Owners[0].Object.Name != "payments" {
		t.Fatalf("owners = %+v, want the payments team", o.Owners)
	}
	if src := o.Owners[0].Sources; len(src) == 0 || src[len(src)-1].Source != "kubernetes-acme" || src[len(src)-1].ObservationID == "" {
		t.Errorf("evidence = %+v, want kubernetes-acme observations", src)
	}
	ms, err = v.store.Mappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var asserted *vocab.Mapping
	for i, m := range ms {
		if m.ID == id && m.Status == vocab.Asserted {
			asserted = &ms[i]
		}
	}
	if asserted == nil || asserted.Author != "ops@acme" || asserted.ValidFrom.IsZero() || asserted.ValidTo != nil {
		t.Errorf("asserted row = %+v, want author, valid_from and no valid_to", asserted)
	}
}

// TestVocabModuleCoreNamespaceRefused: an adapter that publishes terms in
// the core namespace is turned away before its module is stored.
func TestVocabModuleCoreNamespaceRefused(t *testing.T) {
	t.Parallel()
	v := newVocabWorld(t, nil)
	m := describedModule(v)
	m.Namespace = vocab.CoreNamespace
	if err := m.CheckAdapter(); err == nil {
		t.Fatal("a core-namespace adapter module was accepted")
	}
}
