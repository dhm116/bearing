package e2e

import (
	"slices"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/fakes"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
	"bearing.example/pkg/server"
)

const k8sService = model.Kind("k8s.Service")

// vocabRun is the Kubernetes source of Acme booted the way pkg/server/boot
// boots it, once per call to boot: the settings' vocabulary is written to the
// store's kind table, the registry it returns is what the store and the
// queries read with, and the kinds the source keeps come from the booted
// vocabulary, not the read one. The same store lives across boots.
type vocabRun struct {
	*cluster
	applier server.StoreApplier
	q       *query.Querier
	decl    *modelv1alpha1.AdapterDeclaration
}

// newVocabRun is a cluster whose adapter, as the e2e fixture has it, emits
// its workloads as k8s.Service, a configured kind extending Component. The
// declaration lists k8s.Service as optional beside the adapter's own kinds.
func newVocabRun(t *testing.T) *vocabRun {
	t.Helper()
	c := newCluster(t)
	decl := &modelv1alpha1.AdapterDeclaration{Name: "kubernetes", IssuerType: "kubernetes"}
	for _, k := range []model.Kind{model.KindComponent, model.KindEnvironment, model.KindTeam, k8sService} {
		decl.Kinds = append(decl.Kinds, &modelv1alpha1.KindDeclaration{Kind: string(k), Optional: k == k8sService})
	}
	return &vocabRun{cluster: c, decl: decl}
}

// boot syncs the vocabulary (with k8s.Service configured or not) and builds
// the applier and the querier.
func (v *vocabRun) boot(configured bool) {
	v.t.Helper()
	reg := model.Core()
	if configured {
		var err error
		if reg, err = reg.Extend([]model.KindDef{{Name: k8sService, Parent: model.KindComponent}}, nil); err != nil {
			v.t.Fatal(err)
		}
	}
	read, err := v.store.SyncVocabulary(v.t.Context(), reg, v.clock.Now())
	if err != nil {
		v.t.Fatal(err)
	}
	v.store.SetRegistry(read)
	var active []model.Kind
	for _, k := range v.decl.GetKinds() {
		if reg.Valid(model.Kind(k.GetKind())) {
			active = append(active, model.Kind(k.GetKind()))
		}
	}
	v.applier = server.StoreApplier{
		Store: v.store, Registry: read, Authority: map[string]func(model.Key) bool{"kubernetes-acme": authoritative(v.t, "kubernetes")},
		Active: map[string][]model.Kind{"kubernetes-acme": active},
	}
	v.q = &query.Querier{Graph: v.store, Registry: read, Now: v.clock.Now}
}

// fullSync reads the whole cluster and applies it as one complete sync.
func (v *vocabRun) fullSync() {
	v.t.Helper()
	var all []*eventv1alpha1.Observation
	var done *modelv1alpha1.CompleteSync
	cur := ""
	for range 100 {
		res, err := v.a.Sync(v.t.Context(), adapter.SyncParams{Config: v.cfg, Cursor: cur})
		if err != nil {
			v.t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			done = res.CompleteSync
			break
		}
		cur = res.NextCursor
	}
	if done == nil {
		v.t.Fatal("sync did not finish")
	}
	var started time.Time
	for _, o := range all {
		// The fixture's adapter emits its workloads as k8s.Service.
		if e := o.GetData().GetEntity(); e.GetKind() == string(model.KindComponent) {
			e.Kind = string(k8sService)
		}
		if t := o.GetTime().AsTime(); started.IsZero() || t.Before(started) {
			started = t
		}
	}
	var kinds []model.Kind
	for _, k := range done.GetKinds() {
		kinds = append(kinds, model.Kind(k))
	}
	if err := v.applier.Apply(v.t.Context(), "kubernetes-acme", all, server.Completion{Kinds: kinds, Started: started}); err != nil {
		v.t.Fatal(err)
	}
}

func (v *vocabRun) owners() *query.Ownership {
	v.t.Helper()
	o, err := v.q.Owners(v.t.Context(), workload("deployment", "payments", "payments-api"), v.clock.Now(), false)
	if err != nil {
		v.t.Fatal(err)
	}
	return o
}

// TestAcmeConfiguredKindAnswersQuestionOneAndSurvivesBeingDisabled walks a
// configured kind through ADR 10: used, disabled by a reboot without it (its
// observations dropped, its facts kept and shown as of the retirement) and
// enabled again (un-retired, restored by a full sync).
func TestAcmeConfiguredKindAnswersQuestionOneAndSurvivesBeingDisabled(t *testing.T) {
	t.Parallel()
	v := newVocabRun(t)
	team := func(o *query.Ownership) []string {
		var out []string
		for _, f := range o.Owners {
			out = append(out, f.Object.Key)
		}
		return out
	}

	v.boot(true)
	v.fullSync()
	o := v.owners()
	if o.Subject.Kind != string(k8sService) {
		t.Fatalf("payments-api is a %s, want %s", o.Subject.Kind, k8sService)
	}
	if want := []string{"kubernetes:team/payments"}; !slices.Equal(team(o), want) || o.Owners[0].AsOf != nil {
		t.Fatalf("question 1 through k8s.Service: got owners %v (as_of %v), want %v with no as_of", team(o), o.Owners[0].AsOf, want)
	}

	// Reboot without the kind, some time later: the service observations are
	// dropped and a complete sync does not read them as deleted.
	v.clock.Set(v.clock.Now().Add(24 * time.Hour))
	retiredAt := v.clock.Now()
	v.boot(false)
	// Meanwhile the cluster clears billing-core's team label. Were its
	// observation kept, the next sync would close that owned_by fact.
	v.playUntil(fakes.TeamLabelClearedAt)
	before := v.facts(graph.FactFilter{Predicate: "owned_by"})
	v.fullSync()
	if after := v.facts(graph.FactFilter{Predicate: "owned_by"}); len(after) != len(before) {
		t.Fatalf("owned_by facts now %d, want the %d from before: a disabled kind closed facts", len(after), len(before))
	}
	o = v.owners()
	if want := []string{"kubernetes:team/payments"}; !slices.Equal(team(o), want) {
		t.Fatalf("after disabling: got owners %v, want %v still answered", team(o), want)
	}
	if got := o.Owners[0].AsOf; got == nil || !got.Equal(retiredAt) {
		t.Fatalf("after disabling: got as_of %v, want the retirement %v", got, retiredAt)
	}

	// Enable it again: un-retired, and a full sync restores it.
	v.clock.Set(v.clock.Now().Add(24 * time.Hour))
	v.boot(true)
	v.fullSync()
	if got := len(v.facts(graph.FactFilter{Predicate: "owned_by"})); got != len(before)-1 {
		t.Fatalf("after enabling: %d owned_by facts, want %d: the full sync should close the cleared label", got, len(before)-1)
	}
	o = v.owners()
	if o.Owners[0].AsOf != nil || o.Subject.Kind != string(k8sService) {
		t.Fatalf("after enabling: got kind %s, as_of %v, want %s and none", o.Subject.Kind, o.Owners[0].AsOf, k8sService)
	}
}

func (v *vocabRun) facts(f graph.FactFilter) []graph.Fact {
	v.t.Helper()
	f.At = v.clock.Now()
	fs, err := v.store.Facts(v.t.Context(), f)
	if err != nil {
		v.t.Fatal(err)
	}
	return fs
}
