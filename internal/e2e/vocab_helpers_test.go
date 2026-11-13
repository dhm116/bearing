package e2e

import (
	"cmp"
	"encoding/json"
	"testing"

	"bearing.example/adapters/kubernetes"
	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/adapter"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/query"
	"bearing.example/pkg/vocab"
)

// vocabWorld is Acme's cluster read by the Kubernetes adapter in namespaced
// mode (ADR 13 spike), booted the way a server with vocabulary modules would
// boot: modules synced to the term table, suggestions stored as proposed
// mappings, settings mappings as asserted, and the registry built from the
// modules and the mappings in force. The same store lives across boots.
type vocabWorld struct {
	*cluster
	// Modules loaded at every boot besides the core: the adapter's, and an
	// operator module if a test adds one.
	Modules []vocab.Module
	// Settings are the operator's asserted mappings, with full names.
	Settings []vocab.Mapping
	// Sources names the source publishing an adapter module, by namespace;
	// kubernetes-acme when absent.
	Sources map[string]string
	reg     *model.Registry
	q       *query.Querier
}

// newVocabWorld is the cluster with the adapter in namespaced mode; edit
// changes the adapter config first, when not nil.
func newVocabWorld(t *testing.T, edit func(*kubernetes.Config)) *vocabWorld {
	t.Helper()
	c := newCluster(t)
	var cfg kubernetes.Config
	if err := json.Unmarshal(c.cfg, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Namespaced = true
	if edit != nil {
		edit(&cfg)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.cfg = b
	return &vocabWorld{cluster: c, Modules: []vocab.Module{kubernetes.Module()}}
}

// boot syncs the core and every module, stores the modules' suggestions and
// the settings, and builds the registry from the mappings in force now.
func (v *vocabWorld) boot() {
	v.t.Helper()
	ctx, now := v.t.Context(), v.clock.Now()
	for _, m := range append([]vocab.Module{vocab.Core()}, v.Modules...) {
		source := ""
		if m.Owner == vocab.OwnerAdapter {
			source = cmp.Or(v.Sources[m.Namespace], "kubernetes-acme")
		}
		if err := v.store.SyncModule(ctx, m, source, now); err != nil {
			v.t.Fatal(err)
		}
		for _, s := range m.Suggests {
			s.Status, s.Author, s.ValidFrom = vocab.Proposed, source, now
			if err := v.store.PutMapping(ctx, s); err != nil {
				v.t.Fatal(err)
			}
		}
	}
	for _, s := range v.Settings {
		s.Status, s.ValidFrom = vocab.Asserted, now
		if s.Author == "" {
			s.Author = "settings"
		}
		if err := v.store.PutMapping(ctx, s); err != nil {
			v.t.Fatal(err)
		}
	}
	ms, err := v.store.Mappings(ctx)
	if err != nil {
		v.t.Fatal(err)
	}
	reg, err := vocab.Registry(model.Core(), v.Modules, ms, now)
	if err != nil {
		v.t.Fatal(err)
	}
	read, err := v.store.SyncVocabulary(ctx, reg, now)
	if err != nil {
		v.t.Fatal(err)
	}
	v.store.SetRegistry(read)
	v.reg = read
	v.q = &query.Querier{Graph: v.store, Registry: read, Now: v.clock.Now}
}

// sync reads the whole cluster, validates each observation against the
// booted registry, and ingests it as one complete sync. It returns what was
// ingested, for tests that apply rules to it.
func (v *vocabWorld) sync() []*eventv1alpha1.Observation {
	v.t.Helper()
	var all []*eventv1alpha1.Observation
	cur := ""
	for range 100 {
		res, err := v.a.Sync(v.t.Context(), adapter.SyncParams{Config: v.cfg, Cursor: cur})
		if err != nil {
			v.t.Fatal(err)
		}
		all = append(all, res.Observations...)
		if res.Done {
			for _, o := range all {
				if err := model.ValidateObservation(v.reg, o); err != nil {
					v.t.Fatalf("%s: %v", o.GetId(), err)
				}
			}
			if _, err := v.store.Ingest(v.t.Context(), graph.IngestRequest{
				Source: "kubernetes-acme", Observations: all, Authoritative: authoritative(v.t, "kubernetes"),
			}); err != nil {
				v.t.Fatal(err)
			}
			return all
		}
		cur = res.NextCursor
	}
	v.t.Fatal("sync did not finish")
	return nil
}

// paymentsWorkload is payments-api's workload key.
var paymentsWorkload = workload("deployment", "payments", "payments-api")
