package duckstore

import (
	"strings"
	"testing"

	"bearing.example/pkg/model"
)

func TestSyncVocabularyReportsAFailingStepAndReturnsNoRegistry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup []string // run before the sync; nil closes the database
		want  string
	}{
		{"no database", nil, "sync vocabulary: begin:"},
		{"kind table missing", []string{`DROP TABLE kind`}, "read kinds"},
		{"predicate table missing", []string{`DROP TABLE kind_predicate`}, "read predicates"},
		{"unreadable kind row", []string{
			`DROP TABLE kind`,
			`CREATE TABLE kind (name VARCHAR PRIMARY KEY, parent VARCHAR, origin VARCHAR NOT NULL, added_at TIMESTAMPTZ NOT NULL, retired_at VARCHAR)`,
			`INSERT INTO kind VALUES ('x.Old', NULL, 'configured', now(), 'not a time')`,
		}, "read kinds"},
		{"unreadable predicate row", []string{
			`DROP TABLE kind_predicate`,
			`CREATE TABLE kind_predicate (name VARCHAR PRIMARY KEY, relation VARCHAR NOT NULL, type VARCHAR, cardinality VARCHAR NOT NULL, conflict VARCHAR NOT NULL, domain VARCHAR[], range VARCHAR[])`,
			`INSERT INTO kind_predicate VALUES ('p', 'maybe', NULL, 'CARDINALITY_ONE', 'CONFLICT_POLICY_ONE', NULL, NULL)`,
		}, "read predicates"},
		{"kind insert refused", []string{
			`DROP TABLE kind`,
			`CREATE TABLE kind (name VARCHAR PRIMARY KEY, parent VARCHAR, origin VARCHAR NOT NULL, added_at TIMESTAMPTZ NOT NULL, retired_at TIMESTAMPTZ, CHECK (name = 'zzz'))`,
		}, "kind Change:"},
		{"retirement refused", []string{
			`DROP TABLE kind`,
			`CREATE TABLE kind (name VARCHAR PRIMARY KEY, parent VARCHAR, origin VARCHAR NOT NULL, added_at TIMESTAMPTZ NOT NULL, retired_at TIMESTAMPTZ, CHECK (retired_at IS NULL))`,
			`INSERT INTO kind VALUES ('x.Old', NULL, 'configured', now(), NULL)`,
		}, "retire kind x.Old"},
		{"predicate insert refused", []string{
			`DROP TABLE kind_predicate`,
			`CREATE TABLE kind_predicate (name VARCHAR PRIMARY KEY, relation BOOLEAN NOT NULL, type VARCHAR, cardinality VARCHAR NOT NULL, conflict VARCHAR NOT NULL, domain VARCHAR[], range VARCHAR[], CHECK (name = 'zzz'))`,
		}, "predicate account:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t, Config{})
			if tc.setup == nil {
				if err := s.db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for _, q := range tc.setup {
				if _, err := s.db.ExecContext(t.Context(), q); err != nil {
					t.Fatal(err)
				}
			}
			read, err := s.SyncVocabulary(t.Context(), nil, at(1))
			if err == nil || read != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, %v, want a nil registry and an error containing %q", read, err, tc.want)
			}
		})
	}
}

func TestSyncVocabularyNilRegistryReadsAsCoreAndRetiresEveryDroppedKind(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	syncOK(t, s, vocab(t, []model.KindDef{svc, api, unit}), 1)
	read, err := s.SyncVocabulary(t.Context(), nil, at(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.Kind{"acme.Api", "k8s.Service", "systemd.Unit"} {
		if got, ok := read.Retired(k); !ok || !got.Equal(at(2)) {
			t.Errorf("got Retired(%s) = %v, %v, want %v, true", k, got, ok, at(2))
		}
	}
}
