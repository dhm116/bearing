package duckstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"bearing.example/internal/testkit"
	"bearing.example/pkg/store"
	"bearing.example/pkg/store/storetest"
)

func TestEventLogConformance(t *testing.T) {
	t.Parallel()
	storetest.EventLog(t, func(t *testing.T) (store.EventLog, storetest.Clock) {
		clk := testkit.NewClock(time.Time{})
		return newStore(t, Config{Now: clk.Now}), clk
	})
}

// A cancelled context stops every call before it writes anything.
func TestEventLogRefusesACancelledContext(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev := store.Event{ID: "github-acme/d1", Partition: "github-acme", Type: "t", Time: time.Unix(1, 0), Data: []byte("{}")}
	if _, err := s.Append(ctx, []store.Event{ev}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if ps, err := s.Partitions(context.Background()); err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v, want an empty log", ps, err)
	}
}

// A file written before the trace columns existed opens, and its events read
// back without a trace context.
func TestEventLogOpensAFileWithoutTraceColumns(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "old.duckdb")
	ctx := context.Background()
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE event_head (partition VARCHAR PRIMARY KEY, head BIGINT NOT NULL, trimmed BIGINT NOT NULL)`,
		`CREATE TABLE event_log (partition VARCHAR NOT NULL, off BIGINT NOT NULL, id VARCHAR NOT NULL UNIQUE, type VARCHAR NOT NULL, time TIMESTAMPTZ NOT NULL, data BLOB NOT NULL, retain BOOLEAN NOT NULL, appended_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (partition, off))`,
		`INSERT INTO event_head VALUES ('github-acme', 1, 0)`,
		`INSERT INTO event_log VALUES ('github-acme', 1, 'github-acme/d1', 't', now(), '{}'::BLOB, false, now())`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	es, err := s.Read(ctx, "github-acme", 0, 10)
	if err != nil || len(es) != 1 || es[0].TraceParent != "" || es[0].TraceState != "" {
		t.Fatalf("got %+v, %v, want the old event with no trace context", es, err)
	}
}
