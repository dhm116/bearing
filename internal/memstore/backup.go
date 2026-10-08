package memstore

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

// The backup body is the change journal, one JournalEntry per record.
const (
	backupFormat  = "bearing.memstore.journal"
	backupVersion = 1
)

// Backup implements contracts.GraphStore.
func (s *Store) Backup(ctx context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := &modelv1alpha1.BackupHeader{Format: backupFormat, Version: backupVersion, LastSubjectId: s.lastID}
	if !s.head.IsZero() {
		h.Head = timestamppb.New(s.head)
	}
	bw, err := contracts.NewBackupWriter(w, h)
	if err != nil {
		return fmt.Errorf("backup header: %w", err)
	}
	for _, e := range s.journal {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := proto.Marshal(e)
		if err != nil {
			return fmt.Errorf("backup event %s: %w", e.GetChangeSet().GetEventId(), err)
		}
		if err := bw.Record(b); err != nil {
			return fmt.Errorf("backup event %s: %w", e.GetChangeSet().GetEventId(), err)
		}
	}
	return bw.Finish()
}

// Restore implements contracts.GraphStore. It replays the journal, checking
// every apply decides as it did, then seeds the ID source past the last
// restored ID.
func (s *Store) Restore(ctx context.Context, r io.Reader) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.journal) > 0 {
		return errors.New("restore: the store is not empty")
	}
	defer func() {
		if err != nil {
			s.reset()
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
		if _, err := s.apply(e.GetChangeSet(), e); err != nil {
			return fmt.Errorf("restore: record %d: %w", n, err)
		}
	}
	head := time.Time{}
	if h.GetHead() != nil {
		head = h.GetHead().AsTime()
	}
	if !s.head.Equal(head) || s.lastID != h.GetLastSubjectId() {
		return fmt.Errorf("restore: the journal ends at head %s and subject %q; the header says %s and %q", s.head, s.lastID, head, h.GetLastSubjectId())
	}
	if s.lastID != "" {
		if err := s.IDs.Seed(s.lastID); err != nil {
			return fmt.Errorf("restore: seed IDs: %w", err)
		}
	}
	return nil
}
