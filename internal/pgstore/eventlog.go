package pgstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"bearing.example/pkg/contracts"
)

var _ contracts.EventLog = (*Store)(nil)

// trimChunk is how many entries one Trim statement removes, so a long
// backlog is not one long transaction.
const trimChunk = 10_000

// withRetry runs fn, again when the database fails it in a way another try
// may get through (a lost connection, a deadlock). An Append whose commit had
// landed but whose acknowledgement was lost finds its events on the repeat and
// reports them as duplicates; every other operation here is safe to repeat.
func (s *Store) withRetry(ctx context.Context, what string, fn func(context.Context) error) error {
	var last error
	for attempt := range maxApplyAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx)
		if err == nil || !retryable(err) {
			return err
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 5 * time.Millisecond):
		}
	}
	return giveUp(what, maxApplyAttempts, last)
}

// inTx runs fn in one transaction and commits it, trying again as withRetry
// does.
func (s *Store) inTx(ctx context.Context, what string, opts pgx.TxOptions, fn func(context.Context, pgx.Tx) error) error {
	return s.withRetry(ctx, what, func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, opts)
		if err != nil {
			return fmt.Errorf("begin: %w", cleanError(err))
		}
		defer rollback(tx)
		if err := fn(ctx, tx); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit: %w", cleanError(err))
		}
		return nil
	})
}

var readOnly = pgx.TxOptions{AccessMode: pgx.ReadOnly}

func micros(t time.Time) int64 { return t.UTC().Truncate(time.Microsecond).UnixMicro() }

// Append implements contracts.EventLog. One transaction locks the head row of
// each partition the events name, in partition order so two appends never
// wait on each other in a circle, assigns the offsets of the events that are
// new and writes them. The locks last until the commit, so offsets become
// visible in order.
func (s *Store) Append(ctx context.Context, events []contracts.Event) ([]contracts.Appended, error) {
	if err := contracts.CheckEvents(events); err != nil {
		return nil, err
	}
	var out []contracts.Appended
	err := s.withRetry(ctx, "append", func(ctx context.Context) (err error) {
		out, err = s.appendOnce(ctx, events)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) appendOnce(ctx context.Context, events []contracts.Event) ([]contracts.Appended, error) {
	at := micros(s.Now())
	byPart := map[contracts.Partition][]int{}
	for i := range events {
		byPart[events[i].Partition] = append(byPart[events[i].Partition], i)
	}
	parts := make([]contracts.Partition, 0, len(byPart))
	for p := range byPart {
		parts = append(parts, p)
	}
	slices.Sort(parts)

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", cleanError(err))
	}
	defer rollback(tx)

	out := make([]contracts.Appended, len(events))
	var rows eventRows
	heads := make([]int64, len(parts))
	for pi, p := range parts {
		if _, err := tx.Exec(ctx, `INSERT INTO event_partition (name, head) VALUES ($1, 0) ON CONFLICT (name) DO NOTHING`, string(p)); err != nil {
			return nil, fmt.Errorf("create partition: %w", cleanError(err))
		}
		var head int64
		if err := tx.QueryRow(ctx, `SELECT head FROM event_partition WHERE name = $1 FOR UPDATE`, string(p)).Scan(&head); err != nil {
			return nil, fmt.Errorf("lock partition: %w", cleanError(err))
		}
		idx := byPart[p]
		ids := make([]string, len(idx))
		for j, i := range idx {
			ids[j] = events[i].ID
		}
		existing, err := foundIDs(ctx, tx, ids)
		if err != nil {
			return nil, err
		}
		for _, i := range idx {
			e := &events[i]
			off, ok := existing[e.ID]
			if ok {
				out[i] = contracts.Appended{Partition: p, Offset: contracts.Offset(off), Duplicate: true}
				continue
			}
			head++
			existing[e.ID] = head
			out[i] = contracts.Appended{Partition: p, Offset: contracts.Offset(head)}
			rows.add(e, head, at)
		}
		heads[pi] = head
	}
	if err := rows.insert(ctx, tx); err != nil {
		return nil, err
	}
	for pi, p := range parts {
		if _, err := tx.Exec(ctx, `UPDATE event_partition SET head = $2 WHERE name = $1 AND head < $2`, string(p), heads[pi]); err != nil {
			return nil, fmt.Errorf("advance partition: %w", cleanError(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", cleanError(err))
	}
	return out, nil
}

// foundIDs returns the offsets of the IDs that are already in the log.
func foundIDs(ctx context.Context, q querier, ids []string) (map[string]int64, error) {
	rs, err := q.Query(ctx, `SELECT id, off FROM event WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("look for duplicates: %w", cleanError(err))
	}
	defer rs.Close()
	found := map[string]int64{}
	for rs.Next() {
		var id string
		var off int64
		if err := rs.Scan(&id, &off); err != nil {
			return nil, fmt.Errorf("look for duplicates: %w", err)
		}
		found[id] = off
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("look for duplicates: %w", cleanError(err))
	}
	return found, nil
}

// eventRows are the entries one Append writes, as the columns of an unnest.
type eventRows struct {
	part, id, typ         []string
	off, evTime, appended []int64
	retain                []bool
	data                  [][]byte
}

func (r *eventRows) add(e *contracts.Event, off, at int64) {
	r.part = append(r.part, string(e.Partition))
	r.off = append(r.off, off)
	r.id = append(r.id, e.ID)
	r.typ = append(r.typ, e.Type)
	r.evTime = append(r.evTime, micros(e.Time))
	r.appended = append(r.appended, at)
	r.retain = append(r.retain, e.Retain)
	data := e.Data
	if data == nil {
		data = []byte{} // a NULL would break the NOT NULL column
	}
	r.data = append(r.data, data)
}

func (r *eventRows) insert(ctx context.Context, q querier) error {
	if len(r.id) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, `
INSERT INTO event (part, off, id, ev_type, ev_time, appended_at, retain, data)
SELECT * FROM unnest($1::text[], $2::bigint[], $3::text[], $4::text[], $5::bigint[], $6::bigint[], $7::boolean[], $8::bytea[])`,
		r.part, r.off, r.id, r.typ, r.evTime, r.appended, r.retain, r.data)
	if err != nil {
		return fmt.Errorf("insert events: %w", cleanError(err))
	}
	return nil
}

// Read implements contracts.EventLog. It reads in one snapshot: first the
// offsets and sizes of the next entries, to stop before the data limit, then
// the entries themselves.
func (s *Store) Read(ctx context.Context, partition contracts.Partition, after contracts.Offset, limit int) ([]contracts.Entry, error) {
	if err := contracts.CheckRead(partition, after, limit); err != nil {
		return nil, err
	}
	var out []contracts.Entry
	err := s.withRetry(ctx, "read", func(ctx context.Context) error {
		tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return fmt.Errorf("begin: %w", cleanError(err))
		}
		defer rollback(tx)
		rs, err := tx.Query(ctx, `SELECT off, octet_length(data) FROM event WHERE part = $1 AND off > $2 ORDER BY off LIMIT $3`, string(partition), int64(after), limit)
		if err != nil {
			return fmt.Errorf("read sizes: %w", cleanError(err))
		}
		var last int64
		total := 0
		for rs.Next() {
			var off int64
			var size int
			if err := rs.Scan(&off, &size); err != nil {
				rs.Close()
				return fmt.Errorf("read sizes: %w", err)
			}
			if last != 0 && total+size > contracts.MaxReadBytes {
				break
			}
			total += size
			last = off
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return fmt.Errorf("read sizes: %w", cleanError(err))
		}
		out = nil
		if last == 0 {
			return nil
		}
		rs, err = tx.Query(ctx, `
SELECT off, id, ev_type, ev_time, appended_at, retain, data FROM event
WHERE part = $1 AND off > $2 AND off <= $3 ORDER BY off`, string(partition), int64(after), last)
		if err != nil {
			return fmt.Errorf("read entries: %w", cleanError(err))
		}
		defer rs.Close()
		for rs.Next() {
			var off, evTime, appended int64
			e := contracts.Entry{}
			if err := rs.Scan(&off, &e.ID, &e.Type, &evTime, &appended, &e.Retain, &e.Data); err != nil {
				return fmt.Errorf("read entries: %w", err)
			}
			e.Partition, e.Offset = partition, contracts.Offset(off)
			e.Time, e.AppendedAt = time.UnixMicro(evTime).UTC(), time.UnixMicro(appended).UTC()
			out = append(out, e)
		}
		if err := rs.Err(); err != nil {
			return fmt.Errorf("read entries: %w", cleanError(err))
		}
		return nil
	})
	return out, err
}

// Commit implements contracts.EventLog. The head only grows and a visible
// entry has a visible head, so the check needs no lock.
func (s *Store) Commit(ctx context.Context, group string, partition contracts.Partition, offset contracts.Offset) error {
	if err := contracts.CheckCommit(group, partition, offset); err != nil {
		return err
	}
	return s.inTx(ctx, "commit", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var head int64
		err := tx.QueryRow(ctx, `SELECT head FROM event_partition WHERE name = $1`, string(partition)).Scan(&head)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("partition %s: %w", partition, contracts.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read partition: %w", cleanError(err))
		}
		if int64(offset) > head {
			return fmt.Errorf("%w: offset %d is beyond the head, %d", contracts.ErrInvalidRequest, offset, head)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO event_offset (grp, part, off) VALUES ($1, $2, $3)
ON CONFLICT (grp, part) DO UPDATE SET off = GREATEST(event_offset.off, EXCLUDED.off)`,
			group, string(partition), int64(offset)); err != nil {
			return fmt.Errorf("write offset: %w", cleanError(err))
		}
		return nil
	})
}

// Committed implements contracts.EventLog.
func (s *Store) Committed(ctx context.Context, group string, partition contracts.Partition) (contracts.Offset, error) {
	if err := contracts.CheckCommit(group, partition, 0); err != nil {
		return 0, err
	}
	var off int64
	err := s.inTx(ctx, "committed", readOnly, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT off FROM event_offset WHERE grp = $1 AND part = $2`, group, string(partition)).Scan(&off)
		if errors.Is(err, pgx.ErrNoRows) {
			off = 0
			return nil
		}
		if err != nil {
			return fmt.Errorf("read offset: %w", cleanError(err))
		}
		return nil
	})
	return contracts.Offset(off), err
}

// Partitions implements contracts.EventLog. The collation is C so the order
// is the bytes' order, as on every other backend.
func (s *Store) Partitions(ctx context.Context) ([]contracts.PartitionInfo, error) {
	var out []contracts.PartitionInfo
	err := s.inTx(ctx, "partitions", readOnly, func(ctx context.Context, tx pgx.Tx) error {
		rs, err := tx.Query(ctx, `SELECT name, head, trimmed FROM event_partition ORDER BY name COLLATE "C"`)
		if err != nil {
			return fmt.Errorf("read partitions: %w", cleanError(err))
		}
		defer rs.Close()
		out = nil
		for rs.Next() {
			var name string
			var head, trimmed int64
			if err := rs.Scan(&name, &head, &trimmed); err != nil {
				return fmt.Errorf("read partitions: %w", err)
			}
			out = append(out, contracts.PartitionInfo{Partition: contracts.Partition(name), Head: contracts.Offset(head), Trimmed: contracts.Offset(trimmed)})
		}
		return rs.Err()
	})
	return out, err
}

// Trim implements contracts.EventLog. It works one partition at a time and
// in chunks, so it holds no lock on two partitions and no long transaction.
func (s *Store) Trim(ctx context.Context, before time.Time, groups []string) (int, error) {
	if err := contracts.CheckTrim(before, groups); err != nil {
		return 0, err
	}
	cutoff := micros(before)
	if before.UTC().Truncate(time.Microsecond).Before(before.UTC()) {
		cutoff++ // an entry at the cutoff's rounded-down microsecond is before it
	}
	var parts []string
	err := s.inTx(ctx, "trim", readOnly, func(ctx context.Context, tx pgx.Tx) error {
		rs, err := tx.Query(ctx, `SELECT DISTINCT part FROM event WHERE NOT retain AND appended_at < $1`, cutoff)
		if err != nil {
			return fmt.Errorf("find partitions: %w", cleanError(err))
		}
		defer rs.Close()
		parts = nil
		for rs.Next() {
			var p string
			if err := rs.Scan(&p); err != nil {
				return fmt.Errorf("find partitions: %w", err)
			}
			parts = append(parts, p)
		}
		return rs.Err()
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, p := range parts {
		for {
			var n int
			err := s.inTx(ctx, "trim", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
				// bound is the lowest offset a required group committed in
				// the partition (zero for a group with none).
				return tx.QueryRow(ctx, `
WITH b AS (
	SELECT coalesce(min(coalesce(o.off, 0)), 9223372036854775807) AS bound
	FROM unnest($4::text[]) g LEFT JOIN event_offset o ON o.grp = g AND o.part = $1
), d AS (
	DELETE FROM event WHERE (part, off) IN (
		SELECT part, off FROM event
		WHERE part = $1 AND NOT retain AND appended_at < $2 AND off <= (SELECT bound FROM b)
		ORDER BY off LIMIT $3
	) RETURNING off
), u AS (
	UPDATE event_partition SET trimmed = GREATEST(trimmed, (SELECT max(off) FROM d)) WHERE name = $1 AND EXISTS (SELECT 1 FROM d)
)
SELECT count(*) FROM d`, p, cutoff, trimChunk, groups).Scan(&n)
			})
			if err != nil {
				return total, fmt.Errorf("partition %s: %w", p, cleanError(err))
			}
			total += n
			if n < trimChunk {
				break
			}
		}
	}
	return total, nil
}

// Release implements contracts.EventLog. Unknown IDs are skipped.
func (s *Store) Release(ctx context.Context, ids []string) error {
	if err := contracts.CheckRelease(ids); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return s.inTx(ctx, "release", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE event SET retain = false WHERE id = ANY($1::text[]) AND retain`, ids); err != nil {
			return fmt.Errorf("release: %w", cleanError(err))
		}
		return nil
	})
}
