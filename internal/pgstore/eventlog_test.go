package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
)

func TestPostgresEventLogConformance(t *testing.T) {
	t.Parallel()
	conformance.EventLog(t, func(t *testing.T) (contracts.EventLog, conformance.Clock) {
		clk := testkit.NewClock(time.Time{})
		s := newTestStore(t)
		s.Now = clk.Now
		return s, clk
	})
}

func logEvent(partition, id string) contracts.Event {
	return contracts.Event{
		ID: partition + "/" + id, Partition: contracts.Partition(partition),
		Type: "dev.bearing.webhook_received.v1", Time: time.Date(2026, 10, 10, 11, 0, 0, 0, time.UTC), Data: []byte(id),
	}
}

// An acknowledged event is in the database, not in the process: a second
// store on the same schema, as after a restart, reads it, and the bookmark.
func TestPostgresEventLogSurvivesReopening(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := testkit.NewClock(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	s, o := openTestStore(t)
	s.Now = clk.Now
	if _, err := s.Append(ctx, []contracts.Event{logEvent("github-acme", "d1"), logEvent("github-acme", "d2")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "resolver", "github-acme", 1); err != nil {
		t.Fatal(err)
	}
	again := reopen(t, o, clk)
	es, err := again.Read(ctx, "github-acme", 0, 10)
	if err != nil || len(es) != 2 || es[1].ID != "github-acme/d2" || string(es[1].Data) != "d2" {
		t.Fatalf("got %v, %v, want both events", es, err)
	}
	if got, err := again.Committed(ctx, "resolver", "github-acme"); err != nil || got != 1 {
		t.Fatalf("got bookmark %d, %v, want 1", got, err)
	}
	got, err := again.Append(ctx, []contracts.Event{logEvent("github-acme", "d2"), logEvent("github-acme", "d3")})
	if err != nil || !got[0].Duplicate || got[0].Offset != 2 || got[1].Offset != 3 {
		t.Fatalf("got %+v, %v, want d2 a duplicate at 2 and d3 at 3", got, err)
	}
}

// Backup and Restore are of the graph; the log is not theirs to touch.
func TestPostgresRestoreLeavesTheEventLogAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, _ := graphStoreAt(t)
	if _, err := src.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "e1", State: stateEntries(1, "k")}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Append(ctx, []contracts.Event{logEvent("github-acme", "in-source")}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := src.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("github-acme/in-source")) {
		t.Fatal("the backup holds an event")
	}
	dst, _ := graphStoreAt(t)
	if _, err := dst.Append(ctx, []contracts.Event{logEvent("github-acme", "d1")}); err != nil {
		t.Fatal(err)
	}
	if err := dst.Commit(ctx, "resolver", "github-acme", 1); err != nil {
		t.Fatal(err)
	}
	if err := dst.Restore(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	es, err := dst.Read(ctx, "github-acme", 0, 10)
	if err != nil || len(es) != 1 || es[0].ID != "github-acme/d1" {
		t.Fatalf("got %v, %v, want the target's own event after Restore", es, err)
	}
	if got, _ := dst.Committed(ctx, "resolver", "github-acme"); got != 1 {
		t.Fatalf("got bookmark %d after Restore, want 1", got)
	}
}

// Trim works in chunks; more entries than one chunk still all go, and the
// partition remembers the highest offset removed.
func TestPostgresTrimRemovesMoreThanOneChunk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := testkit.NewClock(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	s := newTestStore(t)
	s.Now = clk.Now
	if _, err := s.Append(ctx, []contracts.Event{logEvent("github-acme", "first")}); err != nil {
		t.Fatal(err)
	}
	extra := trimChunk + 5
	err := s.inTx(ctx, "fill", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO event (part, off, id, ev_type, ev_time, appended_at, retain, data)
SELECT 'github-acme', 1 + g, 'github-acme/bulk' || g, 't', 0, 0, false, ''::bytea FROM generate_series(1, $1::int) g`, extra); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE event_partition SET head = $1 WHERE name = 'github-acme'`, 1+extra)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	clk.Set(clk.Now().Add(time.Hour))
	if err := s.Commit(ctx, "applier", "github-acme", contracts.Offset(1+extra)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, []contracts.Event{logEvent("github-acme", "seed")}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Trim(ctx, clk.Now().Add(-30*time.Minute), []string{"applier"})
	if err != nil || n != 1+extra {
		t.Fatalf("Trim = %d, %v, want %d", n, err, 1+extra)
	}
	ps, err := s.Partitions(ctx)
	if err != nil || len(ps) != 1 || ps[0].Trimmed != contracts.Offset(1+extra) || ps[0].Head != contracts.Offset(2+extra) {
		t.Fatalf("got %+v, %v, want trimmed %d and head %d", ps, err, 1+extra, 2+extra)
	}
	if es, _ := s.Read(ctx, "github-acme", 0, 10); len(es) != 1 || es[0].ID != "github-acme/seed" {
		t.Fatalf("got %v, want only the seed entry (appended just now)", es)
	}
}

// Appends to several partitions at once, trims and commits in between, all
// finish: the locks are taken in an order that cannot deadlock.
func TestPostgresEventLogLocksDoNotDeadlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	parts := []string{"a", "b", "c"}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				// Each call names the partitions in a different order.
				var evs []contracts.Event
				for k := range parts {
					p := parts[(k+w)%len(parts)]
					evs = append(evs, logEvent(p, fmt.Sprintf("w%d-%d", w, i)))
				}
				if _, err := s.Append(ctx, evs); err != nil {
					errs <- err
					return
				}
				// Process everything so far, so Trim removes rows while the
				// other appends run.
				ps, err := s.Partitions(ctx)
				if err != nil {
					errs <- err
					return
				}
				for _, p := range ps {
					if err := s.Commit(ctx, "applier", p.Partition, p.Head); err != nil {
						errs <- err
						return
					}
				}
				if _, err := s.Trim(ctx, time.Now().Add(time.Hour), []string{"applier"}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A commit whose answer was lost lands the events; the retry finds them and
// says they are duplicates. A database that keeps failing gives ErrBusy.
func TestPostgresAppendRetriesAndGivesUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	lost := &pgconn.PgError{Code: "08006", Message: "connection failure"}
	probe := &faultPool{}
	if _, err := withPool(s, probe).Append(ctx, []contracts.Event{logEvent("github-acme", "probe")}); err != nil {
		t.Fatal(err)
	}
	f := &faultPool{failAt: probe.calls, lostAck: true, err: lost}
	got, err := withPool(s, f).Append(ctx, []contracts.Event{logEvent("github-acme", "d1")})
	if err != nil || len(got) != 1 || !got[0].Duplicate || got[0].Offset != 2 {
		t.Fatalf("got %+v, %v, want the landed event reported as a duplicate at 2", got, err)
	}
	if es, _ := s.Read(ctx, "github-acme", 0, 10); len(es) != 2 {
		t.Fatalf("got %d entries, want 2", len(es))
	}
	always := &alwaysFailing{err: &pgconn.PgError{Code: "40001", Message: "could not serialize access"}}
	_, err = (&Store{db: always, Now: s.Now}).Append(ctx, []contracts.Event{logEvent("github-acme", "d2")})
	if !errors.Is(err, ErrBusy) || always.n != maxApplyAttempts {
		t.Fatalf("got %v after %d tries, want ErrBusy after %d", err, always.n, maxApplyAttempts)
	}
	permanent := &alwaysFailing{err: &pgconn.PgError{Code: "23505", Message: "duplicate key"}}
	if _, err := (&Store{db: permanent, Now: s.Now}).Read(ctx, "github-acme", 0, 10); err == nil || errors.Is(err, ErrBusy) || permanent.n != 1 {
		t.Fatalf("got %v after %d tries, want a failure at once", err, permanent.n)
	}
}

// Every statement of every event log call may fail: the call then reports it,
// and the log is as it was.
func TestPostgresEventLogEveryStatementMayFail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clk := testkit.NewClock(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	s := newTestStore(t)
	s.Now = clk.Now
	seed := []contracts.Event{logEvent("a", "s1"), logEvent("a", "s2"), logEvent("b", "s1")}
	if _, err := s.Append(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "applier", "a", 2); err != nil {
		t.Fatal(err)
	}
	n := 0
	ops := []struct {
		name string
		// prep makes the call below have something to do.
		prep func(t *testing.T)
		run  func(*Store) error
	}{
		{"Append", nil, func(w *Store) error {
			n++
			_, err := w.Append(ctx, []contracts.Event{logEvent("a", fmt.Sprintf("n%d", n)), logEvent("c", fmt.Sprintf("n%d", n))})
			return err
		}},
		{"Read", nil, func(w *Store) error { _, err := w.Read(ctx, "a", 0, 10); return err }},
		{"Commit", nil, func(w *Store) error { return w.Commit(ctx, "other", "a", 1) }},
		{"Committed", nil, func(w *Store) error { _, err := w.Committed(ctx, "applier", "a"); return err }},
		{"Partitions", nil, func(w *Store) error { _, err := w.Partitions(ctx); return err }},
		{"Release", nil, func(w *Store) error { return w.Release(ctx, []string{"a/s1", "a/never"}) }},
		{"Trim", func(t *testing.T) {
			// A trimmable entry: appended now, committed past, cutoff in the future.
			n++
			if _, err := s.Append(ctx, []contracts.Event{logEvent("t", fmt.Sprintf("n%d", n))}); err != nil {
				t.Fatal(err)
			}
			head, err := s.Partitions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range head {
				if p.Partition == "t" {
					if err := s.Commit(ctx, "applier", "t", p.Head); err != nil {
						t.Fatal(err)
					}
				}
			}
		}, func(w *Store) error {
			_, err := w.Trim(ctx, clk.Now().Add(time.Hour), []string{"applier"})
			return err
		}},
	}
	for _, op := range ops {
		prep := func() {
			if op.prep != nil {
				op.prep(t)
			}
		}
		prep()
		probe := &faultPool{}
		if err := op.run(withPool(s, probe)); err != nil {
			t.Fatalf("%s: a clean call failed: %v", op.name, err)
		}
		if probe.calls == 0 {
			t.Fatalf("%s: counted no statements", op.name)
		}
		for k := 1; k <= probe.calls; k++ {
			prep()
			before := count(t, s, `SELECT count(*) FROM event`)
			if err := op.run(withPool(s, &faultPool{failAt: k})); !errors.Is(err, errInjected) {
				t.Errorf("%s: statement %d of %d failed and the call said %v", op.name, k, probe.calls, err)
			}
			if after := count(t, s, `SELECT count(*) FROM event`); op.name != "Trim" && after != before {
				t.Errorf("%s: statement %d failed and left %d entries, had %d", op.name, k, after, before)
			}
		}
	}
	// The log still answers.
	if _, err := s.Read(ctx, "a", 0, 10); err != nil {
		t.Fatalf("Read after the failures: %v", err)
	}
}

// An append that fails part way consumes no offset and leaves no partition.
func TestPostgresFailedAppendConsumesNoOffset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	probe := &faultPool{}
	if _, err := withPool(s, probe).Append(ctx, []contracts.Event{logEvent("z", "probe")}); err != nil {
		t.Fatal(err)
	}
	fresh := newTestStore(t)
	for k := 1; k <= probe.calls; k++ {
		if _, err := withPool(fresh, &faultPool{failAt: k}).Append(ctx, []contracts.Event{logEvent("z", "d1")}); !errors.Is(err, errInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", k, err)
		}
	}
	if ps, err := fresh.Partitions(ctx); err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v, want no partition after the failed appends", ps, err)
	}
	got, err := fresh.Append(ctx, []contracts.Event{logEvent("z", "d1")})
	if err != nil || got[0].Offset != 1 || got[0].Duplicate {
		t.Fatalf("got %+v, %v, want offset 1", got, err)
	}
}
