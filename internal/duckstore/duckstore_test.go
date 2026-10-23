package duckstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
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
	// rather than crash DuckDB or deadlock, whichever part they use.
	s := newStore(t, Config{})
	if n := s.db.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("got %d max open connections, want 1", n)
	}
	ctx := t.Context()
	ingestOK(t, s, "src", obs("o-a", 0, "Person", "p:a/1"), obs("o-b", 0, "Person", "p:b/1"), obs("o-api", 0, "Component", "k:a/api"))
	recs, err := s.Records(ctx, "Person")
	if err != nil || len(recs) != 2 {
		t.Fatalf("got %+v, %v, want two records", recs, err)
	}
	a, b := recs[0].ID, recs[1].ID
	work := []func(i int) error{
		func(i int) error {
			_, err := s.Append(ctx, []store.Event{{ID: fmt.Sprintf("p/%d", i), Partition: "p", Type: "t", Time: time.Unix(1, 0), Data: []byte("{}")}})
			return err
		},
		func(int) error { _, err := s.Partitions(ctx); return err },
		func(i int) error {
			_, err := s.Ingest(ctx, graph.IngestRequest{Source: "src", Observations: []*eventv1alpha1.Observation{
				obs(fmt.Sprintf("o%d", i), i, "Component", "k:a/api", rel("depends_on", "k:a/db", false)),
			}})
			return err
		},
		func(int) error {
			_, err := s.Link(ctx, []graph.Proposal{{A: a, B: b, Kind: graph.SameAs, Status: graph.Proposed, Rule: "test", Deposit: graph.Deposit{Weight: 1}}})
			return err
		},
		func(int) error { _, err := s.Entity(ctx, "k:a/api"); return err },
		func(int) error { _, err := s.Walk(ctx, graph.WalkRequest{From: "k:a/api"}); return err },
		func(int) error { _, err := s.Links(ctx, graph.LinkFilter{Entity: a}); return err },
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(work))
	for _, w := range work {
		wg.Go(func() {
			for i := range 30 {
				if err := w(i); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent calls did not finish: a deadlock on the one connection")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
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
