package duckstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"bearing.example/pkg/store"
)

// inTx runs fn in a transaction, committing if it returns nil.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Append implements store.EventLog.
func (s *Store) Append(ctx context.Context, events []store.Event) ([]store.Appended, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.CheckEvents(events); err != nil {
		return nil, err
	}
	at := s.now().UTC().Truncate(time.Microsecond)
	s.logWrite.Lock()
	defer s.logWrite.Unlock()
	out := make([]store.Appended, len(events))
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		heads := map[store.Partition]store.Offset{}
		for i, e := range events {
			var off store.Offset
			err := tx.QueryRowContext(ctx, `SELECT off FROM event_log WHERE id = ?`, e.ID).Scan(&off)
			if err == nil {
				out[i] = store.Appended{Partition: e.Partition, Offset: off, Duplicate: true}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("find event %s: %w", e.ID, err)
			}
			head, ok := heads[e.Partition]
			if !ok {
				err := tx.QueryRowContext(ctx, `SELECT head FROM event_head WHERE partition = ?`, string(e.Partition)).Scan(&head)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("partition %s: %w", e.Partition, err)
				}
			}
			head++
			heads[e.Partition] = head
			parent, state := store.CleanTrace(e.TraceParent, e.TraceState)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO event_log (partition, off, id, type, time, data, retain, appended_at, traceparent, tracestate) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))`,
				string(e.Partition), int64(head), e.ID, e.Type, e.Time.UTC().Truncate(time.Microsecond), e.Data, e.Retain, at, parent, state); err != nil {
				return fmt.Errorf("insert event %s: %w", e.ID, err)
			}
			out[i] = store.Appended{Partition: e.Partition, Offset: head}
		}
		for p, head := range heads {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO event_head VALUES (?, ?, 0) ON CONFLICT (partition) DO UPDATE SET head = excluded.head`,
				string(p), int64(head)); err != nil {
				return fmt.Errorf("partition %s head: %w", p, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Read implements store.EventLog.
func (s *Store) Read(ctx context.Context, partition store.Partition, after store.Offset, limit int) ([]store.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.CheckRead(partition, after, limit); err != nil {
		return nil, err
	}
	// The window keeps entries while the running total of Data stays within
	// MaxReadBytes, and always the first.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, time, data, retain, off, appended_at, coalesce(traceparent, ''), coalesce(tracestate, '') FROM (
			SELECT *, sum(octet_length(data)) OVER (ORDER BY off) AS cum, row_number() OVER (ORDER BY off) AS rn
			FROM (SELECT * FROM event_log WHERE partition = ? AND off > ? ORDER BY off LIMIT ?)
		) WHERE rn = 1 OR cum <= ? ORDER BY off`,
		string(partition), int64(after), limit, store.MaxReadBytes)
	if err != nil {
		return nil, fmt.Errorf("read partition %s: %w", partition, err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.Entry
	for rows.Next() {
		e := store.Entry{Event: store.Event{Partition: partition}}
		var off int64
		if err := rows.Scan(&e.ID, &e.Type, &e.Time, &e.Data, &e.Retain, &off, &e.AppendedAt, &e.TraceParent, &e.TraceState); err != nil {
			return nil, fmt.Errorf("read partition %s: %w", partition, err)
		}
		e.Offset = store.Offset(off)
		e.Time, e.AppendedAt = e.Time.UTC(), e.AppendedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// Commit implements store.EventLog.
func (s *Store) Commit(ctx context.Context, group string, partition store.Partition, offset store.Offset) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.CheckCommit(group, partition, offset); err != nil {
		return err
	}
	s.logWrite.Lock()
	defer s.logWrite.Unlock()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var head int64
		err := tx.QueryRowContext(ctx, `SELECT head FROM event_head WHERE partition = ?`, string(partition)).Scan(&head)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("partition %s: %w", partition, store.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("partition %s: %w", partition, err)
		}
		if int64(offset) > head {
			return fmt.Errorf("%w: offset %d is beyond the head, %d", store.ErrInvalidRequest, offset, head)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO event_commit VALUES (?, ?, ?) ON CONFLICT (grp, partition) DO UPDATE SET off = greatest(off, excluded.off)`,
			group, string(partition), int64(offset))
		return err
	})
}

// Committed implements store.EventLog.
func (s *Store) Committed(ctx context.Context, group string, partition store.Partition) (store.Offset, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := store.CheckCommit(group, partition, 0); err != nil {
		return 0, err
	}
	var off int64
	err := s.db.QueryRowContext(ctx, `SELECT off FROM event_commit WHERE grp = ? AND partition = ?`, group, string(partition)).Scan(&off)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("group %s, partition %s: %w", group, partition, err)
	}
	return store.Offset(off), nil
}

// Partitions implements store.EventLog.
func (s *Store) Partitions(ctx context.Context) ([]store.PartitionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT partition, head, trimmed FROM event_head ORDER BY partition`)
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []store.PartitionInfo{}
	for rows.Next() {
		var p store.PartitionInfo
		var head, trimmed int64
		if err := rows.Scan(&p.Partition, &head, &trimmed); err != nil {
			return nil, fmt.Errorf("list partitions: %w", err)
		}
		p.Head, p.Trimmed = store.Offset(head), store.Offset(trimmed)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Trim implements store.EventLog.
func (s *Store) Trim(ctx context.Context, before time.Time, groups []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := store.CheckTrim(before, groups); err != nil {
		return 0, err
	}
	groups = slices.Compact(slices.Sorted(slices.Values(groups)))
	// Stored times are whole microseconds, so rounding the cutoff up keeps "before" exact.
	cutoff := before.UTC().Truncate(time.Microsecond)
	if cutoff.Before(before) {
		cutoff = cutoff.Add(time.Microsecond)
	}
	s.logWrite.Lock()
	defer s.logWrite.Unlock()
	removed := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		removed = 0
		rows, err := tx.QueryContext(ctx, `SELECT partition FROM event_head`)
		if err != nil {
			return fmt.Errorf("list partitions: %w", err)
		}
		var parts []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				_ = rows.Close()
				return err
			}
			parts = append(parts, p)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range parts {
			// Entries above the lowest offset a required group committed stay.
			applied := int64(math.MaxInt64)
			for _, g := range groups {
				var off int64 // zero when the group has no row
				err := tx.QueryRowContext(ctx, `SELECT off FROM event_commit WHERE grp = ? AND partition = ?`, g, p).Scan(&off)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("group %s, partition %s: %w", g, p, err)
				}
				applied = min(applied, off)
			}
			rows, err := tx.QueryContext(ctx,
				`DELETE FROM event_log WHERE partition = ? AND NOT retain AND appended_at < ? AND off <= ? RETURNING off`,
				p, cutoff, applied)
			if err != nil {
				return fmt.Errorf("trim partition %s: %w", p, err)
			}
			n, top := 0, int64(0)
			for rows.Next() {
				var off int64
				if err := rows.Scan(&off); err != nil {
					_ = rows.Close()
					return err
				}
				n, top = n+1, max(top, off)
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("trim partition %s: %w", p, err)
			}
			if n == 0 {
				continue
			}
			removed += n
			if _, err := tx.ExecContext(ctx, `UPDATE event_head SET trimmed = greatest(trimmed, ?) WHERE partition = ?`, top, p); err != nil {
				return fmt.Errorf("trim partition %s: %w", p, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// Release implements store.EventLog.
func (s *Store) Release(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.CheckRelease(ids); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	s.logWrite.Lock()
	defer s.logWrite.Unlock()
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE event_log SET retain = false WHERE id = ?`, id); err != nil {
				return fmt.Errorf("release %s: %w", id, err)
			}
		}
		return nil
	})
}
