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
	"bearing.example/pkg/vocab"
)

var (
	svc    = model.KindDef{Name: "io.k8s:Service", Parent: model.KindComponent}
	api    = model.KindDef{Name: "com.acme:Api", Parent: "io.k8s:Service"}
	unit   = model.KindDef{Name: "io.systemd:Unit", Parent: model.KindComponent}
	attrP  = model.Predicate{Name: "pager_rota", Type: modelv1alpha1.ValueType_VALUE_TYPE_STRING, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_ONE, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_ONE}
	relP   = model.Predicate{Name: "routes_to", Relation: true, Domain: []model.Kind{"io.k8s:Service", "io.systemd:Unit"}, Range: []model.Kind{"io.k8s:Service"}, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_MANY, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET}
	anyRel = model.Predicate{Name: "routes_to", Relation: true, Cardinality: modelv1alpha1.Cardinality_CARDINALITY_MANY, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET}
)

func withKinds(t *testing.T, kinds []model.KindDef, preds ...model.Predicate) *model.Registry {
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
	if err := s.db.QueryRowContext(t.Context(), `SELECT retired_at FROM term WHERE name = ?`, kind).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts == nil
}

func TestSyncVocabularyWritesRowsOnceAndRebootChangesNothing(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	r := withKinds(t, []model.KindDef{svc, api, unit}, attrP, relP)
	syncOK(t, s, r, 1)
	var kinds, preds int
	count := func() {
		t.Helper()
		if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FILTER (type = 'TERM_TYPE_KIND'), count(*) FILTER (type <> 'TERM_TYPE_KIND') FROM term`).Scan(&kinds, &preds); err != nil {
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
	if err := s.db.QueryRowContext(t.Context(), `SELECT added_at FROM term WHERE name = 'com.acme:Api'`).Scan(&addedAt); err != nil || !addedAt.Equal(at(1)) {
		t.Fatalf("got added_at %v, %v, want %v", addedAt, err, at(1))
	}
	if _, ok := read.Retired("com.acme:Api"); ok || !read.Valid("com.acme:Api") {
		t.Fatal("got com.acme:Api retired or invalid after a plain reboot")
	}
}

func TestSyncVocabularyRetiresADroppedKindAndUnretiresItWhenReadded(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	full := withKinds(t, []model.KindDef{svc, api})
	syncOK(t, s, full, 1)

	read := syncOK(t, s, withKinds(t, []model.KindDef{svc}), 5)
	if got, ok := read.Retired("com.acme:Api"); !ok || !got.Equal(at(5)) {
		t.Fatalf("got Retired(com.acme:Api) = %v, %v, want %v, true", got, ok, at(5))
	}
	if !read.Valid("com.acme:Api") || !read.IsA("com.acme:Api", model.KindComponent) || read.Parent("com.acme:Api") != "io.k8s:Service" {
		t.Fatal("the read registry lost the retired kind's parents")
	}
	if _, ok := read.Retired("io.k8s:Service"); ok {
		t.Fatal("got io.k8s:Service retired, want active")
	}

	// A second boot without it keeps the first retirement time.
	read = syncOK(t, s, withKinds(t, []model.KindDef{svc}), 9)
	if got, _ := read.Retired("com.acme:Api"); !got.Equal(at(5)) {
		t.Fatalf("got retirement time %v after a second boot, want %v", got, at(5))
	}

	read = syncOK(t, s, full, 12)
	if _, ok := read.Retired("com.acme:Api"); ok {
		t.Fatal("got com.acme:Api still retired after it was re-added")
	}
	if !retiredNull(t, s, "com.acme:Api") {
		t.Fatal("got retired_at set after re-adding, want NULL")
	}
}

func TestSyncVocabularyRefusesAChangeToAStoredKindOrPredicate(t *testing.T) {
	t.Parallel()
	many := attrP
	many.Cardinality, many.Conflict = modelv1alpha1.Cardinality_CARDINALITY_MANY, modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET
	narrowDomain := relP
	narrowDomain.Domain = []model.Kind{"io.k8s:Service"}
	narrowAny := anyRel
	narrowAny.Range = []model.Kind{"io.k8s:Service"}
	tests := []struct {
		name          string
		first, second *model.Registry
		subject       string
	}{
		{"parent changed", withKinds(t, []model.KindDef{svc}), withKinds(t, []model.KindDef{{Name: "io.k8s:Service", Parent: model.KindPackage}}), "io.k8s:Service"},
		{"parent dropped", withKinds(t, []model.KindDef{svc}), withKinds(t, []model.KindDef{{Name: "io.k8s:Service"}}), "io.k8s:Service"},
		{"cardinality changed", withKinds(t, []model.KindDef{svc}, attrP), withKinds(t, []model.KindDef{svc}, many), "pager_rota"},
		{"domain narrowed", withKinds(t, []model.KindDef{svc, unit}, relP), withKinds(t, []model.KindDef{svc, unit}, narrowDomain), "routes_to"},
		{"any range narrowed", withKinds(t, []model.KindDef{svc}, anyRel), withKinds(t, []model.KindDef{svc}, narrowAny), "routes_to"},
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
	syncOK(t, s, withKinds(t, []model.KindDef{svc, unit}, attrP, relP), 1)
	many := attrP
	many.Cardinality, many.Conflict = modelv1alpha1.Cardinality_CARDINALITY_MANY, modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_SET
	// Adds com.acme:New and drops io.systemd:Unit (kind writes), then fails on pager_rota.
	_, err := s.SyncVocabulary(t.Context(), withKinds(t, []model.KindDef{svc, {Name: "com.acme:New", Parent: model.KindComponent}}, many, anyRel), at(2))
	if !errors.Is(err, store.ErrVocabularyChanged) {
		t.Fatalf("got %v, want ErrVocabularyChanged", err)
	}
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM term WHERE name = 'com.acme:New'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("got %d com.acme:New rows, %v, want the insert rolled back", n, err)
	}
	if !retiredNull(t, s, "io.systemd:Unit") {
		t.Fatal("got io.systemd:Unit retired, want the retirement rolled back")
	}
}

func TestSyncVocabularyAllowsWideningAndADomainKindDroppedWithItsRetirement(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	syncOK(t, s, withKinds(t, []model.KindDef{svc, unit}, relP), 1)

	// unit is dropped and leaves relP's domain with it: not a narrowing.
	narrow := relP
	narrow.Domain = []model.Kind{"io.k8s:Service"}
	read := syncOK(t, s, withKinds(t, []model.KindDef{svc}, narrow), 2)
	if p, _ := read.Predicate("routes_to"); len(p.Domain) != 2 {
		t.Fatalf("got domain %v, want the stored io.systemd:Unit kept for old facts", p.Domain)
	}
	if _, ok := read.Retired("io.systemd:Unit"); !ok {
		t.Fatal("got io.systemd:Unit active, want retired")
	}

	// A wider domain (any kind) is allowed and sticks.
	read = syncOK(t, s, withKinds(t, []model.KindDef{svc, unit}, anyRel), 3)
	if p, _ := read.Predicate("routes_to"); p.Domain != nil {
		t.Fatalf("got domain %v, want any kind", p.Domain)
	}
}

func TestSyncVocabularyKeepsADroppedAdapterAttributeShapeForReads(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	syncOK(t, s, withKinds(t, nil, attrP), 1)
	read := syncOK(t, s, model.Core(), 2)
	p, ok := read.Predicate("pager_rota")
	if !ok || p.Cardinality != attrP.Cardinality || p.Type != attrP.Type || p.Conflict != attrP.Conflict {
		t.Fatalf("got %+v, %v, want the stored shape of pager_rota", p, ok)
	}
}

func TestSyncVocabularyErrorsOnAStoredShapeWithAnUnknownEnumName(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO term (name, namespace, type, origin, value_type, cardinality, conflict, added_at) VALUES ('old_attr', 'com.dougmccall.bearing.core', 'TERM_TYPE_ATTRIBUTE', 'configured', 'NOPE', 'CARDINALITY_ONE', 'CONFLICT_POLICY_ONE', now())`); err != nil {
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
	syncOK(t, s, withKinds(t, []model.KindDef{svc, api, unit}, attrP, relP), 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = newStore(t, Config{Path: path})
	if _, err := s.SyncVocabulary(t.Context(), withKinds(t, []model.KindDef{{Name: "io.k8s:Service", Parent: model.KindPackage}}), at(2)); !errors.Is(err, store.ErrVocabularyChanged) {
		t.Fatalf("got %v after restart, want the stored parent refused", err)
	}
	read := syncOK(t, s, withKinds(t, []model.KindDef{svc, unit}, attrP, relP), 3)
	if got, ok := read.Retired("com.acme:Api"); !ok || !got.Equal(at(3)) {
		t.Fatalf("got Retired(com.acme:Api) = %v, %v, want %v, true", got, ok, at(3))
	}
	if p, ok := read.Predicate("routes_to"); !ok || len(p.Domain) != 2 || len(p.Range) != 1 {
		t.Fatalf("got %+v, %v, want routes_to with its stored domain and range", p, ok)
	}
}

// TestSyncVocabularyRefusesACoreTermThatChangedOrWasDropped plays a core
// module that changed under a stored one: each stored row stands for what an
// older Bearing wrote, and booting today's core must fail naming the term.
func TestSyncVocabularyRefusesACoreTermThatChangedOrWasDropped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, stored, subject string
	}{
		{"core kind dropped", `INSERT INTO term (name, namespace, type, origin, added_at) VALUES ('Widget', 'com.dougmccall.bearing.core', 'TERM_TYPE_KIND', 'core', now())`, "Widget"},
		{"core attribute dropped", `INSERT INTO term (name, namespace, type, origin, value_type, cardinality, conflict, added_at)
			VALUES ('widget_count', 'com.dougmccall.bearing.core', 'TERM_TYPE_ATTRIBUTE', 'core', 'VALUE_TYPE_FLOAT', 'CARDINALITY_ONE', 'CONFLICT_POLICY_NONE', now())`, "widget_count"},
		{"core attribute shape changed", `UPDATE term SET cardinality = 'CARDINALITY_MANY' WHERE name = 'slug'`, "slug"},
		{"core relation domain narrowed", `UPDATE term SET domain = ['Person', 'Team', 'Repository'] WHERE name = 'member_of'`, "member_of"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t, Config{})
			syncOK(t, s, nil, 1)
			if _, err := s.db.ExecContext(t.Context(), tc.stored); err != nil {
				t.Fatal(err)
			}
			_, err := s.SyncVocabulary(t.Context(), nil, at(2))
			if !errors.Is(err, store.ErrVocabularyChanged) || !strings.Contains(err.Error(), tc.subject) {
				t.Fatalf("got %v, want ErrVocabularyChanged naming %s", err, tc.subject)
			}
		})
	}
}

func TestSyncVocabularyRecordsTheCoreModuleAndNamespacesEveryTerm(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	syncOK(t, s, withKinds(t, []model.KindDef{svc}, attrP), 1)
	var owner, version string
	var verified *string
	if err := s.db.QueryRowContext(t.Context(), `SELECT owner, version, verified_by FROM prefix WHERE namespace = ?`, vocab.CoreNamespace).Scan(&owner, &version, &verified); err != nil {
		t.Fatal(err)
	}
	if owner != "core" || version != vocab.Core().Version {
		t.Errorf("got prefix row %s %s, want core %s", owner, version, vocab.Core().Version)
	}
	// A test binary carries no commit, so nothing was checked.
	if want := vocab.Core().Source.Commit != ""; (verified != nil) != want || verified != nil && *verified != "build" {
		t.Errorf("got verified_by %v, want build only when the build has a commit (%q)", verified, vocab.Core().Source.Commit)
	}
	for name, want := range map[string]string{
		"io.k8s:Service": "io.k8s", "Component": vocab.CoreNamespace, "owned_by": vocab.CoreNamespace,
		"pager_rota": vocab.CoreNamespace, // declared bare: read as a core term until adapters publish modules
	} {
		var ns string
		if err := s.db.QueryRowContext(t.Context(), `SELECT namespace FROM term WHERE name = ?`, name).Scan(&ns); err != nil || ns != want {
			t.Errorf("term %s: got namespace %q, %v, want %q", name, ns, err, want)
		}
	}
}

// TestSyncVocabularyRefusesNarrowingACorePredicateAConfiguredKindWidened: a
// configured kind added to a core relation's domain stays in it while the
// kind is configured. A term only grows, now for core terms too, so dropping
// the kind from the relation alone refuses the boot; dropping the kind with it
// is a retirement, not a narrowing.
func TestSyncVocabularyRefusesNarrowingACorePredicateAConfiguredKindWidened(t *testing.T) {
	t.Parallel()
	dep := model.Predicate{
		Name: string(model.RelDependsOn), Relation: true, Domain: []model.Kind{"io.k8s:Service"},
		Cardinality: modelv1alpha1.Cardinality_CARDINALITY_MANY, Conflict: modelv1alpha1.ConflictPolicy_CONFLICT_POLICY_NONE,
	}
	s := newStore(t, Config{})
	syncOK(t, s, withKinds(t, []model.KindDef{{Name: "io.k8s:Service"}}, dep), 1)
	_, err := s.SyncVocabulary(t.Context(), withKinds(t, []model.KindDef{{Name: "io.k8s:Service"}}), at(2))
	if !errors.Is(err, store.ErrVocabularyChanged) || !strings.Contains(err.Error(), "depends_on") || !strings.Contains(err.Error(), "io.k8s:Service") {
		t.Fatalf("got %v, want ErrVocabularyChanged naming depends_on and io.k8s:Service", err)
	}
	syncOK(t, s, nil, 3)
}
