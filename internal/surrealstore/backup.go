package surrealstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
)

// The backup body is the change journal, one JournalEntry per record, as
// in the reference store; the format name differs because a restore
// re-decides each entry under the rules of the store reading it.
const (
	backupFormat  = "bearing.surrealstore.journal"
	backupVersion = 1

	// journalPage is how many journal entries Backup reads at a time.
	journalPage = 50
)

// Backup implements contracts.GraphStore.
func (s *Store) Backup(ctx context.Context, w io.Writer) error {
	meta, _, err := s.readTx(ctx, ``, nil)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	h := &modelv1alpha1.BackupHeader{Format: backupFormat, Version: backupVersion, LastSubjectId: meta.LastID}
	taken := s.Now().UTC()
	head := microTime(meta.Head)
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
	// Entries recorded after the head read above belong to a later backup.
	for from := int64(1); from <= meta.Journal; from += journalPage {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.q.Query(ctx, `SELECT seq, data FROM journal WHERE seq >= $from AND seq < $to ORDER BY seq`,
			map[string]any{"from": from, "to": min(from+journalPage, meta.Journal+1)})
		if err != nil {
			return fmt.Errorf("backup: read journal: %w", err)
		}
		var rows []dataRow
		if err := decode(res[0], &rows); err != nil {
			return fmt.Errorf("backup: decode journal: %w", err)
		}
		for _, r := range rows {
			if err := bw.Record(r.Data); err != nil {
				return fmt.Errorf("backup entry %d: %w", r.Seq, err)
			}
		}
	}
	return bw.Finish()
}

// Restore implements contracts.GraphStore. It replays the journal, checking
// every apply decides as it did, then seeds the ID source past the last
// restored ID. It does not guard against writers: nothing may use the store
// while it runs.
func (s *Store) Restore(ctx context.Context, r io.Reader) (err error) {
	meta, _, err := s.readTx(ctx, ``, nil)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if meta.Journal > 0 {
		return errors.New("restore: the store is not empty")
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
	head, lastID, err := s.position(ctx)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	wantHead := time.Time{}
	if h.GetHead() != nil {
		wantHead = h.GetHead().AsTime()
	}
	if !head.Equal(wantHead) || lastID != h.GetLastSubjectId() {
		return fmt.Errorf("restore: the journal ends at head %s and subject %q; the header says %s and %q", head, lastID, wantHead, h.GetLastSubjectId())
	}
	if lastID != "" {
		if err := s.IDs.Seed(lastID); err != nil {
			return fmt.Errorf("restore: seed IDs: %w", err)
		}
	}
	return nil
}

// replay applies one journal entry at its own record time and commits it.
func (s *Store) replay(ctx context.Context, e *modelv1alpha1.JournalEntry, taken time.Time) error {
	cs := e.GetChangeSet()
	for attempt := 0; attempt < maxApplyAttempts; attempt++ {
		if res, ok, err := s.processed(ctx, cs.GetEventId()); err != nil {
			return err
		} else if ok {
			_ = res
			return fmt.Errorf("event %s: applied twice", cs.GetEventId())
		}
		ld, err := s.load(ctx, applyScope(cs))
		if err != nil {
			return err
		}
		if _, err := ld.scratch.Replay(e, taken); err != nil {
			return err
		}
		switch err := s.commit(ctx, ld, cs); {
		case err == nil:
			return nil
		case errors.Is(err, errConflict):
		default:
			return err
		}
	}
	return fmt.Errorf("event %s: gave up after %d conflicting applies", cs.GetEventId(), maxApplyAttempts)
}

// position returns the head and the last minted subject ID.
func (s *Store) position(ctx context.Context) (time.Time, string, error) {
	meta, _, err := s.readTx(ctx, ``, nil)
	if err != nil {
		return time.Time{}, "", err
	}
	return microTime(meta.Head), meta.LastID, nil
}

// reset empties the graph, as a failed Restore must.
func (s *Store) reset(ctx context.Context) error {
	_, err := s.q.Query(ctx, `
BEGIN TRANSACTION;
DELETE subject;
DELETE merge;
DELETE series;
DELETE series_subject;
DELETE version;
DELETE journal;
DELETE processed_event;
UPDATE meta:graph SET head = 0, last_id = '', merges = 0, journal = 0;
COMMIT TRANSACTION;`, nil)
	return err
}
