package duckstore

import (
	"context"
	"path/filepath"
	"testing"
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO entity VALUES ('e1', 'Team', 'payments', NULL, now())`); err != nil {
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

func TestInMemoryStoreIsSharedAcrossConnections(t *testing.T) {
	ctx := t.Context()
	s := newStore(t, Config{})
	c1, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c1.Close() })
	c2, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	if _, err := c1.ExecContext(ctx, `INSERT INTO entity VALUES ('e1', 'Team', 'payments', NULL, now())`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c2.QueryRowContext(ctx, `SELECT count(*) FROM entity`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("got %d, %v from a second connection, want 1", n, err)
	}
}
