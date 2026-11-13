package duckstore

import (
	"errors"
	"strings"
	"testing"

	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
	"bearing.example/pkg/vocab"
)

func k8sModule(version string, terms ...vocab.Term) vocab.Module {
	return vocab.Module{Owner: vocab.OwnerAdapter, Namespace: "io.k8s", Version: version, Terms: terms}
}

var (
	deployment = vocab.Term{Name: "Deployment", Is: vocab.IsKind}
	replicas   = vocab.Term{Name: "replicas", Is: vocab.IsAttribute, Type: "float", Cardinality: "one", Conflict: "none"}
)

func TestSyncModuleGrowOnly(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	ctx := t.Context()
	if err := s.SyncModule(ctx, k8sModule("0.1.0", deployment), "kubernetes-acme", at(1)); err != nil {
		t.Fatal(err)
	}
	// Growing is fine, and so is a reboot with the same module.
	for range 2 {
		if err := s.SyncModule(ctx, k8sModule("0.2.0", deployment, replicas), "kubernetes-acme", at(2)); err != nil {
			t.Fatal(err)
		}
	}
	reshaped := replicas
	reshaped.Cardinality = "many"
	for name, m := range map[string]vocab.Module{
		"reshaped": k8sModule("0.3.0", deployment, reshaped),
		"dropped":  k8sModule("0.3.0", deployment),
	} {
		t.Run(name, func(t *testing.T) {
			err := s.SyncModule(ctx, m, "kubernetes-acme", at(3))
			if !errors.Is(err, store.ErrVocabularyChanged) || !strings.Contains(err.Error(), "io.k8s:replicas") {
				t.Fatalf("got %v, want ErrVocabularyChanged naming io.k8s:replicas", err)
			}
		})
	}
	// Deprecating is growth.
	dep := replicas
	dep.Deprecated = &vocab.Deprecation{Since: "0.3.0", ReplacedBy: "io.k8s:replica_count"}
	if err := s.SyncModule(ctx, k8sModule("0.3.0", deployment, dep), "kubernetes-acme", at(3)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Terms(ctx, "io.k8s")
	if err != nil || len(rows) != 2 || rows[0].Name != "io.k8s:Deployment" || rows[0].Version != "0.1.0" {
		t.Fatalf("terms = %+v, %v; want Deployment added at 0.1.0 and replicas", rows, err)
	}
}

func TestSyncModuleNamespaceHasOneOwner(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	if err := s.SyncModule(t.Context(), k8sModule("0.1.0", deployment), "kubernetes-acme", at(1)); err != nil {
		t.Fatal(err)
	}
	// A second cluster's adapter is a second source; settings would pin the
	// namespace to a source, or (ADR 13) share it by instance.
	err := s.SyncModule(t.Context(), k8sModule("0.1.0", deployment), "kubernetes-other", at(2))
	if !errors.Is(err, ErrNamespaceOwned) || !strings.Contains(err.Error(), "kubernetes-acme") {
		t.Fatalf("got %v, want ErrNamespaceOwned naming the owner", err)
	}
}

func TestMappingsProposeThenAccept(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	ctx := t.Context()
	sug := vocab.Mapping{
		ID: "k8s-deployment-is-component", Subject: "io.k8s:Deployment", Predicate: vocab.Broader,
		Object: vocab.CoreNamespace + ":Component", Status: vocab.Proposed, Author: "kubernetes-acme",
		Justification: "A Deployment runs one service", ValidFrom: at(1),
	}
	for range 2 { // every boot repeats the suggestion
		if err := s.PutMapping(ctx, sug); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DecideMapping(ctx, sug.ID, vocab.Asserted, "ops@acme", "Each Deployment is one service at Acme", at(5)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMapping(ctx, sug); err != nil { // the next boot suggests it again
		t.Fatal(err)
	}
	ms, err := s.Mappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d rows, want the closed proposal and the assertion: %+v", len(ms), ms)
	}
	p, a := ms[0], ms[1]
	if p.Status != vocab.Proposed || p.ValidTo == nil || !p.ValidTo.Equal(at(5)) {
		t.Errorf("proposal = %+v, want closed at 5", p)
	}
	if a.Status != vocab.Asserted || a.Author != "ops@acme" || !a.ValidFrom.Equal(at(5)) || a.ValidTo != nil || !a.InForce(at(6)) {
		t.Errorf("assertion = %+v, want open from 5 by ops@acme", a)
	}
	if err := s.DecideMapping(ctx, sug.ID, vocab.Asserted, "ops", "", at(6)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deciding twice: got %v, want ErrNotFound", err)
	}
}

// TestSyncVocabularyAcceptsLaterBroader is the ADR 12 blocker: a parent added
// after first boot. For a namespaced term it is a dated mapping, not a frozen
// property, so the kind table takes the new parent.
func TestSyncVocabularyAcceptsLaterBroader(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	mod := k8sModule("0.1.0", deployment)
	first, err := vocab.Registry(model.Core(), []vocab.Module{mod}, nil, at(1))
	if err != nil {
		t.Fatal(err)
	}
	syncOK(t, s, first, 1)
	broader := vocab.Mapping{ID: "m", Subject: "io.k8s:Deployment", Predicate: vocab.Broader, Object: vocab.CoreNamespace + ":Component", Status: vocab.Asserted, ValidFrom: at(2)}
	later, err := vocab.Registry(model.Core(), []vocab.Module{mod}, []vocab.Mapping{broader}, at(3))
	if err != nil {
		t.Fatal(err)
	}
	read := syncOK(t, s, later, 3)
	if !read.IsA("io.k8s:Deployment", model.KindComponent) {
		t.Error("io.k8s:Deployment is not a Component after the mapping")
	}
	// ADR 10's rule still holds for its own configured kinds.
	syncOK(t, s, extendCore(t, []model.KindDef{svc}), 4)
	if _, err := s.SyncVocabulary(t.Context(), extendCore(t, []model.KindDef{{Name: "k8s.Service"}}), at(5)); !errors.Is(err, store.ErrVocabularyChanged) {
		t.Errorf("ADR 10 kind lost its parent: got %v, want ErrVocabularyChanged", err)
	}
}
