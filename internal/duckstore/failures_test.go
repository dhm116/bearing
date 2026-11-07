package duckstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/store"
)

// breakStore runs stmt on the store's connection, to make the next query on
// that table fail the same way every time (a context cancelled while a call
// is in flight does it only by timing).
func breakStore(t *testing.T, s *Store, stmt string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), stmt); err != nil {
		t.Fatal(err)
	}
}

func TestAppendReportsAPartitionLookupFailureAndWritesNothing(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	breakStore(t, s, `ALTER TABLE event_head RENAME TO event_head_gone`)
	ev := store.Event{ID: "github-acme/d1", Partition: "github-acme", Type: "t", Time: time.Unix(1, 0), Data: []byte("{}")}
	_, err := s.Append(t.Context(), []store.Event{ev})
	if err == nil || !strings.Contains(err.Error(), "partition github-acme") {
		t.Fatalf("got %v, want an error naming the partition", err)
	}
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM event_log`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("got %d events, %v, want none after the failed append", n, err)
	}
}

func TestAppendReportsAnEventItCannotLookUpOrInsert(t *testing.T) {
	t.Parallel()
	ev := func(id string) store.Event {
		return store.Event{ID: id, Partition: "github-acme", Type: "t", Time: time.Unix(1, 0), Data: []byte("{}")}
	}
	t.Run("lookup", func(t *testing.T) {
		t.Parallel()
		s := newStore(t, Config{})
		breakStore(t, s, `DROP TABLE event_log`)
		if _, err := s.Append(t.Context(), []store.Event{ev("github-acme/d1")}); err == nil || !strings.Contains(err.Error(), "find event github-acme/d1") {
			t.Fatalf("got %v, want an error naming the event", err)
		}
	})
	t.Run("insert", func(t *testing.T) {
		t.Parallel()
		s := newStore(t, Config{})
		if _, err := s.Append(t.Context(), []store.Event{ev("github-acme/d1")}); err != nil {
			t.Fatal(err)
		}
		// A head behind the log makes the next offset one that is taken.
		breakStore(t, s, `UPDATE event_head SET head = 0`)
		if _, err := s.Append(t.Context(), []store.Event{ev("github-acme/d2")}); err == nil || !strings.Contains(err.Error(), "insert event github-acme/d2") {
			t.Fatalf("got %v, want an error naming the event", err)
		}
	})
}

func TestCommitReportsAPartitionLookupFailure(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	breakStore(t, s, `ALTER TABLE event_head RENAME TO event_head_gone`)
	err := s.Commit(t.Context(), "g", "github-acme", 1)
	if err == nil || errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "partition github-acme") {
		t.Fatalf("got %v, want a lookup failure that is not ErrNotFound", err)
	}
}

func TestPartitionsReportsARowItCannotScan(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	breakStore(t, s, `DROP TABLE event_head`)
	breakStore(t, s, `CREATE TABLE event_head (partition VARCHAR, head VARCHAR, trimmed BIGINT)`)
	breakStore(t, s, `INSERT INTO event_head VALUES ('github-acme', 'not a number', 0)`)
	if _, err := s.Partitions(t.Context()); err == nil || !strings.Contains(err.Error(), "list partitions") {
		t.Fatalf("got %v, want a list partitions error", err)
	}
}

func TestFactsReportsAFailedQuery(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	breakStore(t, s, `DROP TABLE fact`)
	if _, err := s.Facts(t.Context(), graph.FactFilter{}); err == nil || !strings.Contains(err.Error(), "facts:") {
		t.Fatalf("got %v, want a facts error", err)
	}
}

func TestLinksReportsAFailedQuery(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	breakStore(t, s, `DROP TABLE identity_link`)
	if _, err := s.Links(t.Context(), graph.LinkFilter{}); err == nil || !strings.Contains(err.Error(), "links:") {
		t.Fatalf("got %v, want a links error", err)
	}
}

func TestLinksReportsAFailedDepositQuery(t *testing.T) {
	t.Parallel()
	s := ingestStore(t)
	people(t, s, "p:a/1", "p:b/1")
	link(t, s, sameAs("id-1", "id-2", graph.Proposed, "matcher/x"))
	breakStore(t, s, `DROP TABLE link_deposit`)
	if _, err := s.Links(t.Context(), graph.LinkFilter{}); err == nil || !strings.Contains(err.Error(), "deposits") {
		t.Fatalf("got %v, want a deposits error", err)
	}
}

// An ingest that cannot write the observation log or read the facts it would
// close fails as a whole, and says which step failed.
func TestIngestReportsAFailedStepAndWritesNothing(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, table, want string }{
		{"observation log", "observation", "log:"},
		{"open facts", "fact", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := ingestStore(t)
			breakStore(t, s, `DROP TABLE `+tt.table)
			_, err := s.Ingest(t.Context(), graph.IngestRequest{
				Source:       "src",
				Observations: []*eventv1alpha1.Observation{obs("oa", 0, "Person", "p:a/1", attrs("name", "a", "tier", "1"))},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
			var n int
			if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM entity`).Scan(&n); err != nil || n != 0 {
				t.Errorf("got %d entities, %v, want none after the failed ingest", n, err)
			}
		})
	}
}

// A cancelled context stops every event log call before it touches the store.
func TestEventLogCallsReturnTheContextErrorWhenCancelled(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func() error{
		"read":       func() error { _, err := s.Read(ctx, "p", 0, 1); return err },
		"commit":     func() error { return s.Commit(ctx, "g", "p", 0) },
		"committed":  func() error { _, err := s.Committed(ctx, "g", "p"); return err },
		"partitions": func() error { _, err := s.Partitions(ctx); return err },
		"trim":       func() error { _, err := s.Trim(ctx, time.Now(), nil); return err },
		"release":    func() error { return s.Release(ctx, []string{"x"}) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want context.Canceled", err)
			}
		})
	}
}

// A transaction that cannot begin fails the call that wanted it.
func TestReadAndIngestFailWhenTheTransactionCannotBegin(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Run("read", func(t *testing.T) {
		t.Parallel()
		_, err := read(ctx, s, func(querier) (int, error) { return 1, nil })
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "begin") {
			t.Fatalf("got %v, want a begin error wrapping context.Canceled", err)
		}
	})
	for name, call := range map[string]func() error{
		"ingest":     func() error { _, err := s.Ingest(ctx, graph.IngestRequest{Source: "src"}); return err },
		"link":       func() error { _, err := s.Link(ctx, nil); return err },
		"close link": func() error { return s.CloseLink(ctx, "l1", time.Now()) },
		"decide":     func() error { _, err := s.Decide(ctx, graph.Decision{ID: "d1", Actor: "a"}); return err },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := call(); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "begin") {
				t.Fatalf("got %v, want a begin error wrapping context.Canceled", err)
			}
		})
	}
}

func TestRecordsReportsAFailedQuery(t *testing.T) {
	t.Parallel()
	s := newStore(t, Config{})
	breakStore(t, s, `DROP TABLE alias`)
	if _, err := s.Records(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "records:") {
		t.Fatalf("got %v, want a records error", err)
	}
}
