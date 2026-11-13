package duckstore

import (
	"context"
	"strings"
	"testing"

	"bearing.example/pkg/vocab"
)

func TestVocabStoreReportsAFailingStep(t *testing.T) {
	t.Parallel()
	mod := k8sModule("0.1.0", deployment)
	sug := vocab.Mapping{ID: "m", Subject: "io.k8s:Deployment", Predicate: vocab.Broader, Object: "x:Y", Status: vocab.Proposed, Target: &vocab.Target{Kind: "k", Key: "t"}, ValidFrom: at(1)}
	syncMod := func(ctx context.Context, s *Store) error { return s.SyncModule(ctx, mod, "src", at(1)) }
	tests := []struct {
		name  string
		setup []string // nil closes the database
		call  func(context.Context, *Store) error
		want  string
	}{
		{"sync: no database", nil, syncMod, "sync module: begin"},
		{"sync: bad module", []string{}, func(ctx context.Context, s *Store) error {
			return s.SyncModule(ctx, vocab.Module{Namespace: "k8s"}, "", at(1))
		}, "invalid namespace"},
		{"sync: no prefix table", []string{`DROP TABLE prefix`}, syncMod, "sync module io.k8s"},
		{"sync: prefix refused", []string{`DROP TABLE prefix`, `CREATE TABLE prefix (namespace VARCHAR PRIMARY KEY, owner VARCHAR, source VARCHAR, version VARCHAR CHECK (version = 'x'), repo VARCHAR, ref VARCHAR, sha256 VARCHAR, verified_by VARCHAR)`}, syncMod, "sync module io.k8s"},
		{"sync: no term table", []string{`DROP TABLE term`}, syncMod, "sync module io.k8s"},
		{"sync: unreadable term", []string{`DROP TABLE term`, `CREATE TABLE term (name VARCHAR PRIMARY KEY, namespace VARCHAR, is_a VARCHAR, shape INTEGER, version VARCHAR, deprecated JSON, added_at TIMESTAMPTZ)`, `INSERT INTO term VALUES ('io.k8s:Deployment', 'io.k8s', 'kind', NULL, '0', NULL, now())`}, syncMod, "sync module io.k8s"},
		{"sync: term refused", []string{`DROP TABLE term`, `CREATE TABLE term (name VARCHAR PRIMARY KEY, namespace VARCHAR, is_a VARCHAR, shape VARCHAR, version VARCHAR CHECK (version = 'x'), deprecated JSON, added_at TIMESTAMPTZ)`}, syncMod, "term io.k8s:Deployment"},
		{"put: no mapping table", []string{`DROP TABLE mapping`}, func(ctx context.Context, s *Store) error { return s.PutMapping(ctx, sug) }, "put mapping m"},
		{"decide: no database", nil, func(ctx context.Context, s *Store) error {
			return s.DecideMapping(ctx, "m", vocab.Asserted, "a", "", at(2))
		}, "decide mapping m"},
		{"decide: no mapping table", []string{`DROP TABLE mapping`}, func(ctx context.Context, s *Store) error {
			return s.DecideMapping(ctx, "m", vocab.Asserted, "a", "", at(2))
		}, "decide mapping m"},
		{"mappings: no table", []string{`DROP TABLE mapping`}, func(ctx context.Context, s *Store) error { _, err := s.Mappings(ctx); return err }, "mappings"},
		{"mappings: bad target", []string{`INSERT INTO mapping VALUES ('m', 1, 's', 'broader', 'o', '[1]', false, 'proposed', 'a', 'j', now(), NULL)`}, func(ctx context.Context, s *Store) error {
			_, err := s.Mappings(ctx)
			return err
		}, "m target"},
		{"terms: no table", []string{`DROP TABLE term`}, func(ctx context.Context, s *Store) error { _, err := s.Terms(ctx, "io.k8s"); return err }, "terms"},
		{"rule counts: no table", []string{`DROP TABLE fact_source`}, func(ctx context.Context, s *Store) error { _, err := s.RuleCounts(ctx); return err }, "rule counts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t, Config{})
			if tc.setup == nil {
				_ = s.Close()
			}
			for _, stmt := range tc.setup {
				execSQL(t, s, stmt)
			}
			if err := tc.call(t.Context(), s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestPutMappingStoresATarget(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	m := vocab.Mapping{
		ID: "r-team", Version: 2, Subject: "io.k8s:Deployment", Predicate: vocab.Derives, Object: "c:owned_by",
		Target: &vocab.Target{Kind: "c:Team", Key: "team:{{ .labels.team }}"}, Status: vocab.Asserted, Author: "ops", ValidFrom: at(1),
	}
	if err := s.PutMapping(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	ms, err := s.Mappings(t.Context())
	if err != nil || len(ms) != 1 || ms[0].Target == nil || ms[0].Target.Key != m.Target.Key || ms[0].Version != 2 {
		t.Fatalf("got %+v, %v; want the rule with its target", ms, err)
	}
}
