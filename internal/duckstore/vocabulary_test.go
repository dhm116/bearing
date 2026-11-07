package duckstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/model"
	"bearing.example/pkg/store"
)

var (
	svc    = model.KindDef{Name: "k8s.Service", Parent: model.KindComponent}
	api    = model.KindDef{Name: "acme.Api", Parent: "k8s.Service"}
	unit   = model.KindDef{Name: "systemd.Unit", Parent: model.KindComponent}
	attrP  = model.Predicate{Name: "pager_rota", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE}
	relP   = model.Predicate{Name: "routes_to", Relation: true, Domain: []model.Kind{"k8s.Service", "systemd.Unit"}, Range: []model.Kind{"k8s.Service"}, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_MANY, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET}
	anyRel = model.Predicate{Name: "routes_to", Relation: true, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_MANY, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET}
)

func vocab(t *testing.T, kinds []model.KindDef, preds ...model.Predicate) *model.Registry {
	t.Helper()
	r, err := model.Core().Extend(kinds, preds)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func syncOK(t *testing.T, s *Store, r *model.Registry, n int) *model.Registry {
	t.Helper()
	read, err := s.SyncVocabulary(t.Context(), r, at(n))
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func retiredNull(t *testing.T, s *Store, kind string) bool {
	t.Helper()
	var ts *time.Time
	if err := s.db.QueryRowContext(t.Context(), `SELECT retired_at FROM kind WHERE name = ?`, kind).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts == nil
}

func TestSyncVocabularyWritesRowsOnceAndRebootChangesNothing(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	r := vocab(t, []model.KindDef{svc, api, unit}, attrP, relP)
	syncOK(t, s, r, 1)
	var kinds, preds int
	count := func() {
		t.Helper()
		if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM kind), (SELECT count(*) FROM kind_predicate)`).Scan(&kinds, &preds); err != nil {
			t.Fatal(err)
		}
	}
	count()
	if kinds != len(r.Kinds()) || preds != len(r.Predicates()) {
		t.Fatalf("got %d kinds and %d predicates, want %d and %d", kinds, preds, len(r.Kinds()), len(r.Predicates()))
	}
	read := syncOK(t, s, r, 2)
	wantK, wantP := kinds, preds
	count()
	if kinds != wantK || preds != wantP {
		t.Fatalf("reboot changed row counts to %d and %d, want %d and %d", kinds, preds, wantK, wantP)
	}
	var addedAt time.Time
	if err := s.db.QueryRowContext(t.Context(), `SELECT added_at FROM kind WHERE name = 'acme.Api'`).Scan(&addedAt); err != nil || !addedAt.Equal(at(1)) {
		t.Fatalf("got added_at %v, %v, want %v", addedAt, err, at(1))
	}
	if _, ok := read.Retired("acme.Api"); ok || !read.Valid("acme.Api") {
		t.Fatal("got acme.Api retired or invalid after a plain reboot")
	}
}

func TestSyncVocabularyRetiresADroppedKindAndUnretiresItWhenReadded(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	full := vocab(t, []model.KindDef{svc, api})
	syncOK(t, s, full, 1)

	read := syncOK(t, s, vocab(t, []model.KindDef{svc}), 5)
	if got, ok := read.Retired("acme.Api"); !ok || !got.Equal(at(5)) {
		t.Fatalf("got Retired(acme.Api) = %v, %v, want %v, true", got, ok, at(5))
	}
	if !read.Valid("acme.Api") || !read.IsA("acme.Api", model.KindComponent) || read.Parent("acme.Api") != "k8s.Service" {
		t.Fatal("the read registry lost the retired kind's parents")
	}
	if _, ok := read.Retired("k8s.Service"); ok {
		t.Fatal("got k8s.Service retired, want active")
	}

	// A second boot without it keeps the first retirement time.
	read = syncOK(t, s, vocab(t, []model.KindDef{svc}), 9)
	if got, _ := read.Retired("acme.Api"); !got.Equal(at(5)) {
		t.Fatalf("got retirement time %v after a second boot, want %v", got, at(5))
	}

	read = syncOK(t, s, full, 12)
	if _, ok := read.Retired("acme.Api"); ok {
		t.Fatal("got acme.Api still retired after it was re-added")
	}
	if !retiredNull(t, s, "acme.Api") {
		t.Fatal("got retired_at set after re-adding, want NULL")
	}
}

func TestSyncVocabularyRefusesAChangeToAStoredKindOrPredicate(t *testing.T) {
	t.Parallel()
	many := attrP
	many.Cardinality, many.Conflict = modelv1alpha1.Cardinality_CARDINALITY_MANY, modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET
	narrowDomain := relP
	narrowDomain.Domain = []model.Kind{"k8s.Service"}
	narrowAny := anyRel
	narrowAny.Range = []model.Kind{"k8s.Service"}
	tests := []struct {
		name          string
		first, second *model.Registry
		subject       string
	}{
		{"parent changed", vocab(t, []model.KindDef{svc}), vocab(t, []model.KindDef{{Name: "k8s.Service", Parent: model.KindPackage}}), "k8s.Service"},
		{"parent dropped", vocab(t, []model.KindDef{svc}), vocab(t, []model.KindDef{{Name: "k8s.Service"}}), "k8s.Service"},
		{"cardinality changed", vocab(t, []model.KindDef{svc}, attrP), vocab(t, []model.KindDef{svc}, many), "pager_rota"},
		{"domain narrowed", vocab(t, []model.KindDef{svc, unit}, relP), vocab(t, []model.KindDef{svc, unit}, narrowDomain), "routes_to"},
		{"any range narrowed", vocab(t, []model.KindDef{svc}, anyRel), vocab(t, []model.KindDef{svc}, narrowAny), "routes_to"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t, Config{})
			syncOK(t, s, tc.first, 1)
			_, err := s.SyncVocabulary(t.Context(), tc.second, at(2))
			if !errors.Is(err, store.ErrVocabularyChanged) || !strings.Contains(err.Error(), tc.subject) {
				t.Fatalf("got %v, want ErrVocabularyChanged naming %s", err, tc.subject)
			}
			// The refusal rolled back: the old vocabulary still boots.
			syncOK(t, s, tc.first, 3)
		})
	}
}

func TestSyncVocabularyRefusalRollsBackKindWritesMadeBeforeIt(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	syncOK(t, s, vocab(t, []model.KindDef{svc, unit}, attrP, relP), 1)
	many := attrP
	many.Cardinality, many.Conflict = modelv1alpha1.Cardinality_CARDINALITY_MANY, modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET
	// Adds acme.New and drops systemd.Unit (kind writes), then fails on pager_rota.
	_, err := s.SyncVocabulary(t.Context(), vocab(t, []model.KindDef{svc, {Name: "acme.New", Parent: model.KindComponent}}, many, anyRel), at(2))
	if !errors.Is(err, store.ErrVocabularyChanged) {
		t.Fatalf("got %v, want ErrVocabularyChanged", err)
	}
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM kind WHERE name = 'acme.New'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("got %d acme.New rows, %v, want the insert rolled back", n, err)
	}
	if !retiredNull(t, s, "systemd.Unit") {
		t.Fatal("got systemd.Unit retired, want the retirement rolled back")
	}
}

func TestSyncVocabularyAllowsWideningAndADomainKindDroppedWithItsRetirement(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	syncOK(t, s, vocab(t, []model.KindDef{svc, unit}, relP), 1)

	// unit is dropped and leaves relP's domain with it: not a narrowing.
	narrow := relP
	narrow.Domain = []model.Kind{"k8s.Service"}
	read := syncOK(t, s, vocab(t, []model.KindDef{svc}, narrow), 2)
	if p, _ := read.Predicate("routes_to"); len(p.Domain) != 2 {
		t.Fatalf("got domain %v, want the stored systemd.Unit kept for old facts", p.Domain)
	}
	if _, ok := read.Retired("systemd.Unit"); !ok {
		t.Fatal("got systemd.Unit active, want retired")
	}

	// A wider domain (any kind) is allowed and sticks.
	read = syncOK(t, s, vocab(t, []model.KindDef{svc, unit}, anyRel), 3)
	if p, _ := read.Predicate("routes_to"); p.Domain != nil {
		t.Fatalf("got domain %v, want any kind", p.Domain)
	}
}

func TestSyncVocabularyKeepsADroppedAdapterAttributeShapeForReads(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	syncOK(t, s, vocab(t, nil, attrP), 1)
	read := syncOK(t, s, model.Core(), 2)
	p, ok := read.Predicate("pager_rota")
	if !ok || p.Cardinality != attrP.Cardinality || p.Type != attrP.Type || p.Conflict != attrP.Conflict {
		t.Fatalf("got %+v, %v, want the stored shape of pager_rota", p, ok)
	}
}

func TestSyncVocabularyErrorsOnAStoredShapeWithAnUnknownEnumName(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO kind_predicate (name, relation, type, cardinality, conflict) VALUES ('old_attr', false, 'NOPE', 'CARDINALITY_ONE', 'CONFLICT_POLICY_ONE')`); err != nil {
		t.Fatal(err)
	}
	_, err := s.SyncVocabulary(t.Context(), model.Core(), at(1))
	if err == nil || !strings.Contains(err.Error(), "old_attr") {
		t.Fatalf("got %v, want an error naming old_attr", err)
	}
}

func TestSyncVocabularyKeepsTheTablesAcrossARestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bearing.duckdb")
	s, err := Open(t.Context(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	syncOK(t, s, vocab(t, []model.KindDef{svc, api, unit}, attrP, relP), 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = newStore(t, Config{Path: path})
	if _, err := s.SyncVocabulary(t.Context(), vocab(t, []model.KindDef{{Name: "k8s.Service", Parent: model.KindPackage}}), at(2)); !errors.Is(err, store.ErrVocabularyChanged) {
		t.Fatalf("got %v after restart, want the stored parent refused", err)
	}
	read := syncOK(t, s, vocab(t, []model.KindDef{svc, unit}, attrP, relP), 3)
	if got, ok := read.Retired("acme.Api"); !ok || !got.Equal(at(3)) {
		t.Fatalf("got Retired(acme.Api) = %v, %v, want %v, true", got, ok, at(3))
	}
	if p, ok := read.Predicate("routes_to"); !ok || len(p.Domain) != 2 || len(p.Range) != 1 {
		t.Fatalf("got %+v, %v, want routes_to with its stored domain and range", p, ok)
	}
}
