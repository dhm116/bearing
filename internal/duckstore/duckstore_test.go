package duckstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"bearing.example/pkg/store"
)

// newStore opens an in-memory store that is closed when the test ends.
func newStore(t *testing.T, c Config) *Store {
	t.Helper()
	s, err := Open(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestOpenKeepsDataAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bearing.duckdb")
	s, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO entity VALUES ('e1', 'Team', 'payments', now(), 'e1')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = newStore(t, Config{Path: path})
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM entity`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("got %d entities, %v, want 1", n, err)
	}
}

func TestConcurrentReadsAndWritesFinish(t *testing.T) {
	// The store runs on one connection (see Open); concurrent callers queue
	// rather than crash DuckDB or deadlock.
	s := newStore(t, Config{})
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() {
		for i := range 50 {
			if _, err := s.Append(ctx, []store.Event{{ID: fmt.Sprintf("p/%d", i), Partition: "p", Type: "t", Time: time.Unix(1, 0), Data: []byte("{}")}}); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Go(func() {
		for range 50 {
			if _, err := s.Partitions(ctx); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestOpenAddsQualifiersToAnOldFile(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := schema.ReadFile("10_graph.sql") // a file made before 30_qualifiers.sql
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(old)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, Config{Path: path})
	if _, err := s.db.ExecContext(ctx, `SELECT qualifiers FROM fact`); err != nil {
		t.Fatalf("got %v, want the qualifiers column", err)
	}
}
