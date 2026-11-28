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
		{"prefix table missing", []string{`DROP TABLE prefix`}, "prefix com.dougmccall.bearing.core"},
		{"term table missing", []string{`DROP TABLE term`}, "read kinds"},
		{"unreadable kind row", []string{
			`DROP TABLE term`,
			`CREATE TABLE term (name VARCHAR PRIMARY KEY, namespace VARCHAR NOT NULL, type VARCHAR NOT NULL, origin VARCHAR NOT NULL, parent VARCHAR, value_type VARCHAR, cardinality VARCHAR, conflict VARCHAR, domain VARCHAR[], range VARCHAR[], added_at TIMESTAMPTZ NOT NULL, retired_at VARCHAR)`,
			`INSERT INTO term (name, namespace, type, origin, added_at, retired_at) VALUES ('x:Old', 'x', 'TERM_TYPE_KIND', 'configured', now(), 'not a time')`,
		}, "read kinds"},
		{"unreadable predicate row", []string{
			`INSERT INTO term (name, namespace, type, origin, added_at) VALUES ('p', 'x', 'TERM_TYPE_ATTRIBUTE', 'configured', now())`,
		}, "read predicates"},
		{"kind insert refused", []string{
			`DROP TABLE term`,
			`CREATE TABLE term (name VARCHAR PRIMARY KEY, namespace VARCHAR NOT NULL, type VARCHAR NOT NULL, origin VARCHAR NOT NULL, parent VARCHAR, value_type VARCHAR, cardinality VARCHAR, conflict VARCHAR, domain VARCHAR[], range VARCHAR[], added_at TIMESTAMPTZ NOT NULL, retired_at TIMESTAMPTZ, CHECK (name = 'zzz'))`,
		}, "kind Change:"},
		{"retirement refused", []string{
			`DROP TABLE term`,
			`CREATE TABLE term (name VARCHAR PRIMARY KEY, namespace VARCHAR NOT NULL, type VARCHAR NOT NULL, origin VARCHAR NOT NULL, parent VARCHAR, value_type VARCHAR, cardinality VARCHAR, conflict VARCHAR, domain VARCHAR[], range VARCHAR[], added_at TIMESTAMPTZ NOT NULL, retired_at TIMESTAMPTZ, CHECK (retired_at IS NULL))`,
			`INSERT INTO term (name, namespace, type, origin, added_at) VALUES ('x:Old', 'x', 'TERM_TYPE_KIND', 'configured', now())`,
		}, "retire kind x:Old"},
		{"predicate insert refused", []string{
			`DROP TABLE term`,
			`CREATE TABLE term (name VARCHAR PRIMARY KEY, namespace VARCHAR NOT NULL, type VARCHAR NOT NULL, origin VARCHAR NOT NULL, parent VARCHAR, value_type VARCHAR, cardinality VARCHAR, conflict VARCHAR, domain VARCHAR[], range VARCHAR[], added_at TIMESTAMPTZ NOT NULL, retired_at TIMESTAMPTZ, CHECK (type = 'TERM_TYPE_KIND'))`,
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
	syncOK(t, s, withKinds(t, []model.KindDef{svc, api, unit}), 1)
	read, err := s.SyncVocabulary(t.Context(), nil, at(2))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.Kind{"com.acme:Api", "io.k8s:Service", "io.systemd:Unit"} {
		if got, ok := read.Retired(k); !ok || !got.Equal(at(2)) {
			t.Errorf("got Retired(%s) = %v, %v, want %v, true", k, got, ok, at(2))
		}
	}
}
