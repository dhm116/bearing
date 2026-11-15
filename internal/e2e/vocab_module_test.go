package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
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

func (v *vocabWorld) isComponent(name string) bool {
	v.t.Helper()
	ms, err := v.q.Find(v.t.Context(), name, "Component", 0)
	if err != nil {
		v.t.Fatal(err)
	}
	return slices.ContainsFunc(ms, func(m query.Match) bool { return m.Name == name })
}

// related is "name (predicate)" per step, sorted, for one predicate and direction.
func (v *vocabWorld) related(ref, pred, dir string) (out []string, steps []query.Step) {
	v.t.Helper()
	r, err := v.q.Related(v.t.Context(), ref, pred, dir, 1, time.Time{})
	if err != nil {
		v.t.Fatal(err)
	}
	for _, s := range r.Steps {
		end := s.Fact.Object
		if dir == "in" {
			end = &s.Fact.Subject
		}
		out = append(out, end.Name+" ("+fmtProtocol(s.Fact.Qualifiers)+")")
	}
	slices.Sort(out)
	return out, r.Steps
}

// TestVocabModuleAcceptMappingsAfterBoot is ADR 13's "accept later" path: the
// adapter ships its module in Describe and its suggestions arrive proposed.
// Until the operator accepts them the adapter's terms mean nothing to the
// core; once accepted, with no change to the adapter, core questions answer
// from them and name the adapter's term behind each answer.
func TestVocabModuleAcceptMappingsAfterBoot(t *testing.T) {
	t.Parallel()
	v := newVocabWorld(t, nil)
	v.Settings = []vocab.Mapping{teamRule(1, teamKey)}
	mod := describedModule(v)
	v.Modules = []vocab.Module{mod}
	v.boot()
	v.sync()
	ctx := t.Context()
	billing := workload("deployment", "billing", "billing-api")

	ms, err := v.store.Mappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range mod.Suggests {
		var rows []vocab.Mapping
		for _, m := range ms {
			if m.ID == s.ID {
				rows = append(rows, m)
			}
		}
		if len(rows) != 1 || rows[0].Status != vocab.Proposed || rows[0].Subject != s.Subject || rows[0].Object != s.Object {
			t.Errorf("suggestion %s rows = %+v, want one proposed row", s.ID, rows)
		}
	}
	if v.isComponent("payments-api") {
		t.Error("payments-api is a Component before the mapping is accepted")
	}
	if got, _ := v.related(billing, "depends_on", "out"); len(got) != 0 {
		t.Errorf("billing-api depends_on = %q before accepting, want none", got)
	}
	if got, _ := v.related(paymentsWorkload, "defined_in", "out"); len(got) != 0 {
		t.Errorf("payments-api defined_in = %q before accepting, want none", got)
	}
	o, err := v.q.Owners(ctx, paymentsWorkload, v.clock.Now(), false)
	if err != nil || len(o.Owners) != 1 || o.Owners[0].Object == nil || o.Owners[0].Object.Name != "payments" {
		t.Fatalf("owners = %+v, %v; want the payments team from the rule, no mapping needed", o, err)
	}

	v.clock.Advance(time.Hour)
	for _, s := range mod.Suggests {
		if err := v.store.DecideMapping(ctx, s.ID, vocab.Asserted, "ops@acme", "Reviewed at Acme: "+s.Justification, v.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	v.clock.Advance(time.Minute)
	v.boot() // same store, same module: the accepted mappings now apply
	v.sync()

	if !v.isComponent("payments-api") {
		t.Error("payments-api is not a Component after the mapping is accepted")
	}
	// What the existing dependency tests expect of billing-api (payments-api
	// has no dependencies in the Acme cluster).
	got, steps := v.related(billing, "depends_on", "out")
	if want := []string{"ledger (http)", "postgres (postgres)"}; !slices.Equal(got, want) {
		t.Fatalf("billing-api depends_on %q, want %q", got, want)
	}
	for _, s := range steps {
		f := s.Fact
		if f.Predicate != "io.k8s:depends_on" {
			t.Errorf("stored predicate = %q, want io.k8s:depends_on behind the core term", f.Predicate)
		}
		if len(f.Sources) == 0 || f.Sources[0].Source != "kubernetes-acme" || f.Sources[0].ObservationID == "" {
			t.Errorf("evidence = %+v, want kubernetes-acme observations", f.Sources)
		}
	}
	if got, _ := v.related(workload("statefulset", "billing", "postgres"), "depends_on", "in"); !slices.Equal(got, []string{"billing-api (postgres)"}) {
		t.Errorf("postgres is depended on by %q, want billing-api", got)
	}
	repos, _ := v.related(paymentsWorkload, "defined_in", "out")
	if len(repos) != 1 {
		t.Fatalf("payments-api defined_in = %q, want the payments repo", repos)
	}
	if r, err := v.q.Related(ctx, paymentsWorkload, "defined_in", "out", 1, time.Time{}); err != nil || !strings.Contains(r.Steps[0].Fact.Object.Name, "payments-api") {
		t.Errorf("defined_in = %+v, %v; want acme/payments-api", r, err)
	}

	ms, err = v.store.Mappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	asserted := 0
	for _, m := range ms {
		if m.Status != vocab.Asserted || m.ID == "r-team" {
			continue
		}
		asserted++
		if m.Author != "ops@acme" || m.Justification == "" || m.ValidFrom.IsZero() || m.ValidTo != nil {
			t.Errorf("asserted %s = %+v, want author, justification, valid_from and no valid_to", m.ID, m)
		}
	}
	if asserted != len(mod.Suggests) {
		t.Errorf("asserted rows = %d, want %d", asserted, len(mod.Suggests))
	}
}

// TestVocabModuleRefusesShapeMismatchAtBoot: accepting depends_on before the
// workload kinds are Components is refused when the registry is built, and
// the error names the mapping.
func TestVocabModuleRefusesShapeMismatchAtBoot(t *testing.T) {
	t.Parallel()
	v := newVocabWorld(t, nil)
	mod := describedModule(v)
	const id = "k8s-depends-on-is-depends-on"
	var accepted []vocab.Mapping
	for _, s := range mod.Suggests {
		if s.ID == id {
			s.Status, s.Author, s.ValidFrom = vocab.Asserted, "ops@acme", v.clock.Now()
			accepted = append(accepted, s)
		}
	}
	if len(accepted) != 1 {
		t.Fatalf("module suggests %d mappings named %s, want 1", len(accepted), id)
	}
	_, err := vocab.Registry(model.Core(), []vocab.Module{mod}, accepted, v.clock.Now())
	if err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("err = %v, want a refusal naming %s", err, id)
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
