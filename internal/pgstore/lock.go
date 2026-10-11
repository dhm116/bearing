package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInstanceLocked is returned by LockInstance when another server already
// holds the lock for this schema.
var ErrInstanceLocked = errors.New("pgstore: another server is using this schema")

// lockClass is the first key of the advisory lock, "BEAR" in ASCII, so the
// lock does not collide with other users of the database's advisory locks.
// The second key is a hash of the schema name.
const lockClass = 0x42454152

// acquirer is the part of *pgxpool.Pool that takes a connection for good.
type acquirer interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// InstanceLock is the advisory lock that makes one server the only one using
// a schema. PostgreSQL holds it for the session of one connection that the
// lock keeps out of the pool, and drops it when that connection ends, so a
// crashed server never leaves it behind.
type InstanceLock struct {
	conn   *pgxpool.Conn
	schema string
}

// LockInstance takes the lock without waiting. It returns ErrInstanceLocked
// when a server already holds it. Until the server has a lease on each
// partition of the event log, this is what keeps two servers from consuming
// one partition out of order (docs/spec/contracts.md, "EventLog").
func (s *Store) LockInstance(ctx context.Context) (*InstanceLock, error) {
	a, ok := s.db.(acquirer)
	if !ok {
		return nil, errors.New("pgstore: the connection pool cannot hold a lock")
	}
	conn, err := a.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgstore: lock: %w", cleanError(err))
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::int, hashtext($2))`, lockClass, s.schema).Scan(&got); err != nil {
		conn.Release()
		return nil, fmt.Errorf("pgstore: lock: %w", cleanError(err))
	}
	if !got {
		conn.Release()
		return nil, ErrInstanceLocked
	}
	return &InstanceLock{conn: conn, schema: s.schema}, nil
}

// Held reports whether the lock is still held: the connection answers and
// PostgreSQL still lists the lock for its session. A server that cannot
// show it should stop, since another may have started (the connection was
// lost and the lock went with it).
func (l *InstanceLock) Held(ctx context.Context) error {
	var n int
	err := l.conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks
WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
  AND classid = $1::oid AND objid = hashtext($2)::oid AND objsubid = 2`, lockClass, l.schema).Scan(&n)
	if err != nil {
		return fmt.Errorf("pgstore: lock: %w", cleanError(err))
	}
	if n == 0 {
		return errors.New("pgstore: the instance lock is gone")
	}
	return nil
}

// Release drops the lock. If PostgreSQL cannot be asked to, it closes the
// connection, which drops it as well.
func (l *InstanceLock) Release(ctx context.Context) error {
	var ok bool
	err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1::int, hashtext($2))`, lockClass, l.schema).Scan(&ok)
	if err != nil || !ok {
		_ = l.conn.Hijack().Close(ctx)
		if err != nil {
			return fmt.Errorf("pgstore: unlock: %w", cleanError(err))
		}
		return nil
	}
	l.conn.Release()
	return nil
}
