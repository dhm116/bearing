package pgstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"bearing.example/internal/testkit"
)

func TestLockInstanceAdmitsOneServerPerSchema(t *testing.T) {
	ctx := context.Background()
	first, o := openTestStore(t)
	second := reopen(t, o, testkit.NewClock(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)))

	l1, err := first.LockInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Held(ctx); err != nil {
		t.Fatalf("a lock just taken is not held: %v", err)
	}
	if _, err := second.LockInstance(ctx); !errors.Is(err, ErrInstanceLocked) {
		t.Fatalf("a second server on the same schema: got %v, want ErrInstanceLocked", err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatal(err)
	}
	l2, err := second.LockInstance(ctx)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	if err := l2.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLockInstanceIsPerSchema(t *testing.T) {
	ctx := context.Background()
	a, _ := openTestStore(t)
	b, _ := openTestStore(t)
	la, err := a.LockInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer la.Release(ctx) //nolint:errcheck // the test is over
	lb, err := b.LockInstance(ctx)
	if err != nil {
		t.Fatalf("a server on another schema: %v", err)
	}
	defer lb.Release(ctx) //nolint:errcheck // the test is over
}

func TestLockInstanceIsLostWithItsConnection(t *testing.T) {
	ctx := context.Background()
	first, o := openTestStore(t)
	second := reopen(t, o, testkit.NewClock(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)))
	l, err := first.LockInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// End the lock's connection from another one, as a lost network or a
	// restarted server would.
	var pid int
	if err := l.conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	pool, ok := first.db.(*pgxpool.Pool)
	if !ok {
		t.Fatalf("got a %T, want the pool", first.db)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	if err := l.Held(ctx); err == nil {
		t.Fatal("a lock whose connection is gone reports held")
	}
	// Releasing a lost lock still gives the connection back to the pool, or
	// closing the store would wait for it.
	if err := l.Release(ctx); err == nil {
		t.Error("releasing a lock whose connection is gone reports no error")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		l2, err := second.LockInstance(ctx)
		if err == nil {
			_ = l2.Release(ctx)
			return
		}
		if !errors.Is(err, ErrInstanceLocked) || time.Now().After(deadline) {
			t.Fatalf("after the connection ended: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
