package pgstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// The backup body is the change journal, one JournalEntry per record, as
// in the reference store; the format name differs because a restore
// re-decides each entry under the rules of the store reading it.
const (
	backupFormat  = "bearing.pgstore.journal"
	backupVersion = 1

	// journalPage is how many journal entries Backup reads at a time.
	journalPage = 20
)

// restoreLock is the advisory lock a Restore holds while it runs, so two
// cannot clear each other's work, and a marker left by a crashed one can be
// told from a restore in progress.
const restoreLock = "bearing:restore"

// Backup implements contracts.GraphStore. It reads the journal a page at a
// time, each in its own snapshot: journal rows are never changed, so the
// entries up to the head it read first are the same in every page, and a
// row that is missing makes the backup fail instead of coming out short.
func (s *Store) Backup(ctx context.Context, w io.Writer) error {
	var meta metaRow
	if err := s.readTx(ctx, func(q querier) (err error) {
		meta, err = readMeta(ctx, q, false)
		return err
	}); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if meta.restoring {
		return fmt.Errorf("backup: %w", ErrRestoring)
	}
	h := &modelv1alpha1.BackupHeader{Format: backupFormat, Version: backupVersion, LastSubjectId: meta.lastID}
	taken := s.Now().UTC()
	head := microTime(meta.head)
	if !head.IsZero() {
		h.Head = timestamppb.New(head)
		if head.After(taken) { // the clock stepped back since the last apply
			taken = head
		}
	}
	h.TakenAt = timestamppb.New(taken)
	bw, err := contracts.NewBackupWriter(w, h)
	if err != nil {
		return fmt.Errorf("backup header: %w", err)
	}
	for from := int64(1); from <= meta.journal; from += journalPage {
		if err := ctx.Err(); err != nil {
			return err
		}
		to := min(from+journalPage, meta.journal+1)
		var entries [][]byte
		err := s.readTx(ctx, func(q querier) error {
			rows, err := q.Query(ctx, `SELECT seq, data FROM journal WHERE seq >= $1 AND seq < $2 ORDER BY seq`, from, to)
			if err != nil {
				return err
			}
			defer rows.Close()
			want := from
			for rows.Next() {
				var (
					seq  int64
					data []byte
				)
				if err := rows.Scan(&seq, &data); err != nil {
					return err
				}
				if seq != want {
					return fmt.Errorf("the journal has entry %d where entry %d should be", seq, want)
				}
				want++
				entries = append(entries, data)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if want != to {
				return fmt.Errorf("the journal ends at entry %d; the head says it holds %d", want-1, to-1)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("backup: read journal: %w", err)
		}
		for i, data := range entries {
			if err := bw.Record(data); err != nil {
				return fmt.Errorf("backup entry %d: %w", from+int64(i), err)
			}
		}
	}
	return bw.Finish()
}

// Restore implements contracts.GraphStore. It replays the journal, checking
// every apply decides as it did, then seeds the ID source past the last
// restored ID. The store is marked as restoring while it runs, so a restore
// that dies leaves a store that refuses every other operation until Restore
// is run again, which starts over. Nothing may use the store while it runs.
func (s *Store) Restore(ctx context.Context, r io.Reader) (err error) {
	br, err := contracts.NewBackupReader(r)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	h := br.Header
	if h.GetFormat() != backupFormat || h.GetVersion() != backupVersion {
		return fmt.Errorf("restore: backup format %q version %d; this store reads %q version %d", h.GetFormat(), h.GetVersion(), backupFormat, backupVersion)
	}
	if h.GetTakenAt() == nil {
		return errors.New("restore: the backup has no taken_at")
	}
	taken := h.GetTakenAt().AsTime()

	// A transaction that does nothing but hold the lock until Restore ends,
	// or the process dies.
	lock, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("restore: %w", cleanError(err))
	}
	defer rollback(lock)
	var got bool
	if err := lock.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1 || current_schema(), 0))`, restoreLock).Scan(&got); err != nil {
		return fmt.Errorf("restore: lock: %w", err)
	}
	if !got {
		return errors.New("restore: another restore is running")
	}
	if err := s.beginRestore(ctx); err != nil {
		return err
	}
	defer func() {
		p := recover()
		if p != nil || err != nil {
			// A fresh context: the restore may have failed because ctx did.
			if rerr := s.reset(context.WithoutCancel(ctx)); rerr != nil {
				err = errors.Join(err, fmt.Errorf("restore: reset: %w", rerr))
			}
		}
		if p != nil {
			panic(p)
		}
	}()
	for n := 1; ; n++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		rec, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		e := &modelv1alpha1.JournalEntry{}
		if err := proto.Unmarshal(rec, e); err != nil {
			return fmt.Errorf("restore: record %d: %w", n, err)
		}
		if err := s.replay(ctx, e, taken); err != nil {
			return fmt.Errorf("restore: record %d: %w", n, err)
		}
	}
	wantHead := time.Time{}
	if h.GetHead() != nil {
		wantHead = h.GetHead().AsTime()
	}
	lastID, err := s.finishRestore(ctx, wantHead, h.GetLastSubjectId())
	if err != nil {
		return err
	}
	if lastID != "" {
		if err := s.IDs.Seed(lastID); err != nil {
			return fmt.Errorf("restore: seed IDs: %w", err)
		}
	}
	return nil
}

// withHeadLock runs fn in a transaction that holds the head row's lock,
// trying again when the database fails the transaction in a way another try
// may not.
func (s *Store) withHeadLock(ctx context.Context, fn func(tx pgx.Tx, meta metaRow) error) error {
	var last error
	for attempt := range maxApplyAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := func() error {
			tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				return fmt.Errorf("begin: %w", cleanError(err))
			}
			defer rollback(tx)
			meta, err := readMeta(ctx, tx, true)
			if err != nil {
				return err
			}
			if err := fn(tx, meta); err != nil {
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			return nil
		}()
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
	return giveUp("restore", maxApplyAttempts, last)
}

// beginRestore checks the store is empty, or holds what an earlier restore
// left (the lock says that restore is gone), clears it, and marks the store
// as restoring.
func (s *Store) beginRestore(ctx context.Context) error {
	err := s.withHeadLock(ctx, func(tx pgx.Tx, meta metaRow) error {
		if meta.journal > 0 && !meta.restoring {
			return errors.New("the store is not empty")
		}
		if err := truncate(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE meta SET restoring = true`)
		return err
	})
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// finishRestore checks the journal ends where the backup's header says and
// clears the restoring mark. It returns the last minted subject ID.
func (s *Store) finishRestore(ctx context.Context, wantHead time.Time, wantLast string) (lastID string, err error) {
	err = s.withHeadLock(ctx, func(tx pgx.Tx, meta metaRow) error {
		head := microTime(meta.head)
		if !head.Equal(wantHead) || meta.lastID != wantLast {
			return fmt.Errorf("the journal ends at head %s and subject %q; the header says %s and %q", head, meta.lastID, wantHead, wantLast)
		}
		lastID = meta.lastID
		_, err := tx.Exec(ctx, `UPDATE meta SET restoring = false`)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("restore: %w", err)
	}
	return lastID, nil
}

// replay applies one journal entry at its own record time and commits it.
func (s *Store) replay(ctx context.Context, e *modelv1alpha1.JournalEntry, taken time.Time) error {
	cs := e.GetChangeSet()
	return s.withHeadLock(ctx, func(tx pgx.Tx, meta metaRow) error {
		if _, ok, err := processed(ctx, tx, cs.GetEventId()); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("event %s: applied twice", cs.GetEventId())
		}
		meta.restoring = false // this is the restore
		ld, err := load(ctx, tx, meta, applyScope(cs))
		if err != nil {
			return err
		}
		if _, err := ld.scratch.Replay(e, taken); err != nil {
			return err
		}
		return commit(ctx, tx, ld, cs)
	})
}

// truncate empties the graph.
func truncate(ctx context.Context, q querier) error {
	_, err := q.Exec(ctx, `TRUNCATE subject, merge_record, unmerge_record, component, series, series_subject, version, journal, audit_record;
UPDATE meta SET head = 0, last_id = '', merges = 0, unmerges = 0, journal = 0, audit_seq = 0, audit_hash = ''::bytea, audit_time = 0`)
	return err
}

// reset empties the graph and clears the restoring mark, as a failed
// Restore must. TRUNCATE takes no time to speak of, however much it clears.
func (s *Store) reset(ctx context.Context) error {
	return s.withHeadLock(ctx, func(tx pgx.Tx, _ metaRow) error {
		if err := truncate(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE meta SET restoring = false`)
		return err
	})
}
