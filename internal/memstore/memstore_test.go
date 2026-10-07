package memstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
	"bearing.example/pkg/model"
)

func newTestStore() (*Store, *testkit.FakeClock) {
	clk := testkit.NewClock(time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC))
	s := New()
	s.Now, s.IDs = clk.Now, testkit.NewUUIDv7s(clk.Now)
	return s, clk
}

func TestConformance(t *testing.T) {
	conformance.GraphStore(t, func(*testing.T) (contracts.GraphStore, conformance.Clock, conformance.IDs) {
		clk := testkit.NewClock(time.Time{})
		s := New()
		ids := testkit.NewUUIDv7s(clk.Now)
		s.Now, s.IDs = clk.Now, ids
		return s, clk, ids
	})
}

func TestVectorConformance(t *testing.T) {
	conformance.VectorIndex(t, func(*testing.T) contracts.VectorIndex { return New() })
}

func mintTeam(event string) *modelv1alpha1.ChangeSet {
	return &modelv1alpha1.ChangeSet{EventId: event, Mints: []*modelv1alpha1.Mint{
		{Ref: "new:a", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION},
	}}
}

// fixedIDs issues one ID over and over, or panics with it.
type fixedIDs struct {
	id     string
	panics bool
}

func (f fixedIDs) NewIDAt(time.Time) string {
	if f.panics {
		panic(f.id)
	}
	return f.id
}

func (fixedIDs) Seed(string) error { return nil }

// Subject IDs are compared as text to keep them increasing, which only
// works for canonical lowercase UUIDv7s; one stamped after its apply's record
// time would also break the order between IDs and record times.
func TestApplyRefusesBadMintedIDs(t *testing.T) {
	ctx := context.Background()
	for name, id := range map[string]string{
		"short":           "0192b1c4",
		"no hyphens":      "0192b1c4x0000x7000x8000x000000000000",
		"upper case":      "0192B1C4-0000-7000-8000-000000000000",
		"version 4":       "0192b1c4-0000-4000-8000-000000000000",
		"from the future": "01a0e5de-b890-7000-8000-000000000000",
		"empty":           "",
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestStore()
			s.IDs = fixedIDs{id: id}
			if res, err := s.Apply(ctx, mintTeam("e")); err == nil {
				t.Fatalf("got %v, want an error for subject ID %q", res.Subjects, id)
			}
			if head, _ := s.Head(ctx); !head.IsZero() {
				t.Fatalf("got head %s, want the failed apply unrecorded", head)
			}
		})
	}
}

func TestApplyRefusesAnIDNotAfterTheLast(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore()
	first, err := s.Apply(ctx, mintTeam("e1"))
	if err != nil {
		t.Fatal(err)
	}
	s.IDs = fixedIDs{id: string(first.Subjects["new:a"])}
	cs := mintTeam("e2")
	cs.BaseRecordedAt = timestamppb.New(first.RecordedAt)
	if _, err := s.Apply(ctx, cs); err == nil || !strings.Contains(err.Error(), "is not after") {
		t.Fatalf("got %v, want an error for reusing %s", err, first.Subjects["new:a"])
	}
}

// The clock moves on between an apply's reads of it, as a real one does
// during a large apply; minted IDs are stamped with the record time anyway.
func TestApplyMintsWhileTheClockTicks(t *testing.T) {
	ctx := context.Background()
	clk := testkit.NewClock(time.Date(2026, 9, 28, 1, 30, 2, 0, time.UTC))
	s := New()
	s.Now = func() time.Time {
		clk.Advance(time.Millisecond)
		return clk.Now()
	}
	s.IDs = testkit.NewUUIDv7s(s.Now)
	big, err := anypb.New(wrapperspb.Bytes(make([]byte, 3<<20)))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		cs := mintTeam(fmt.Sprintf("e%d", i))
		if i%2 == 1 {
			cs.State = []*modelv1alpha1.StateEntry{{Key: "k", Value: big}}
		}
		if head, _ := s.Head(ctx); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		res, err := s.Apply(ctx, cs)
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		id := string(res.Subjects["new:a"])
		if at, err := model.UUIDv7Time(id); err != nil || at.After(res.RecordedAt) {
			t.Fatalf("apply %d: got ID %s stamped %s, %v, want no later than %s", i, id, at, err, res.RecordedAt)
		}
	}
}

// A panic mid-apply undoes what the apply did and frees the store.
func TestApplyUndoesAPanickingApply(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore()
	cs := mintTeam("e")
	cs.Mints = append(cs.Mints, &modelv1alpha1.Mint{Ref: "new:b", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION})
	ids := &panicOnSecond{next: s.IDs}
	s.IDs = ids
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("got no panic, want the ID source's")
			}
		}()
		_, _ = s.Apply(ctx, cs)
	}()
	if _, err := s.Subject(ctx, contracts.SubjectID(ids.first), time.Time{}); ids.first == "" || !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("got %v, want the first mint %q undone", err, ids.first)
	}
	s.IDs = ids.next
	if res, err := s.Apply(ctx, cs); err != nil || len(res.Minted) != 2 {
		t.Fatalf("got %+v, %v, want the event applied after the panic", res, err)
	}
}

type panicOnSecond struct {
	next  IDSource
	first string
}

func (p *panicOnSecond) NewIDAt(at time.Time) string {
	if p.first != "" {
		panic("second ID")
	}
	p.first = p.next.NewIDAt(at)
	return p.first
}

func (p *panicOnSecond) Seed(last string) error { return p.next.Seed(last) }

func TestApplyAndRestoreStopWhenCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s, _ := newTestStore()
	if _, err := s.Apply(cancelled, mintTeam("e")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if _, err := s.Apply(context.Background(), mintTeam("e")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Backup(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(cancelled, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	dst, _ := newTestStore()
	if err := dst.Restore(cancelled, &buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if head, _ := dst.Head(context.Background()); !head.IsZero() {
		t.Fatalf("got head %s, want an empty store", head)
	}
}

// failingWriter accepts n bytes, then fails.
type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		return 0, errors.New("disk full")
	}
	w.n -= len(p)
	return len(p), nil
}

func TestBackupReportsAWriteFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore()
	if _, err := s.Apply(ctx, mintTeam("e")); err != nil {
		t.Fatal(err)
	}
	var full bytes.Buffer
	if err := s.Backup(ctx, &full); err != nil {
		t.Fatal(err)
	}
	// Failing in the header, in the record and in the trailer.
	for _, n := range []int{0, 60, full.Len() - 1} {
		if err := s.Backup(ctx, &failingWriter{n: n}); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("writer failing after %d of %d bytes: got %v, want the write error", n, full.Len(), err)
		}
	}
}

// failingSeed panics when Restore seeds it, after every record has replayed.
type failingSeed struct{ IDSource }

func (failingSeed) Seed(string) error { panic("seed failed") }

// A panic mid-restore leaves the store empty, so a retry can restore.
func TestRestoreResetsAfterAPanic(t *testing.T) {
	ctx := context.Background()
	src, clk := newTestStore()
	for _, event := range []string{"e1", "e2"} {
		cs := mintTeam(event)
		if head, _ := src.Head(ctx); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		clk.Advance(time.Second)
		if _, err := src.Apply(ctx, cs); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := src.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	dst, dclk := newTestStore()
	dclk.Set(clk.Now())
	orig := dst.IDs
	dst.IDs = failingSeed{orig}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("got no panic, want the ID source's")
			}
		}()
		_ = dst.Restore(ctx, bytes.NewReader(buf.Bytes()))
	}()
	if head, _ := dst.Head(ctx); !head.IsZero() {
		t.Fatalf("got head %s after the panic, want an empty store", head)
	}
	dst.IDs = orig
	if err := dst.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("retry after the panic: %v", err)
	}
}

// journal backs up s and returns its entries, for tests to tamper with.
func journal(t *testing.T, s *Store) []*modelv1alpha1.JournalEntry {
	t.Helper()
	var buf bytes.Buffer
	if err := s.Backup(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	br, err := contracts.NewBackupReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var out []*modelv1alpha1.JournalEntry
	for {
		rec, err := br.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		e := &modelv1alpha1.JournalEntry{}
		if err := proto.Unmarshal(rec, e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
}

// backupOf writes entries as a backup taken at taken (a zero taken leaves
// it out), whose header matches the last entry.
func backupOf(t *testing.T, entries []*modelv1alpha1.JournalEntry, taken time.Time) *bytes.Buffer {
	t.Helper()
	h := &modelv1alpha1.BackupHeader{Format: backupFormat, Version: backupVersion}
	if !taken.IsZero() {
		h.TakenAt = timestamppb.New(taken)
	}
	for _, e := range entries {
		h.Head = e.GetChangeSet().GetRecordedAt()
		for _, m := range e.GetMinted() {
			h.LastSubjectId = m.GetSubjectId()
		}
	}
	var buf bytes.Buffer
	bw, err := contracts.NewBackupWriter(&buf, h)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := proto.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := bw.Record(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Finish(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// retime moves an entry to record time at, everywhere the store stamps it.
func retime(e *modelv1alpha1.JournalEntry, at time.Time) {
	e.ChangeSet.RecordedAt = timestamppb.New(at)
	for _, m := range e.GetMinted() {
		m.MintedAt = e.GetChangeSet().GetRecordedAt()
	}
}

// A backup whose checksum is right but whose journal is wrong, or would be
// decided differently today, restores nothing.
func TestRestoreRejectsABadJournal(t *testing.T) {
	ctx := context.Background()
	src, clk := newTestStore()
	ids := []string{}
	for _, cs := range []*modelv1alpha1.ChangeSet{
		{EventId: "e0"},
		mintTeam("e1"),
		{EventId: "e2", Mints: []*modelv1alpha1.Mint{{Ref: "new:b", Kind: "Team", Rule: modelv1alpha1.MintRule_MINT_RULE_OBSERVATION}}},
	} {
		if head, _ := src.Head(ctx); !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		clk.Advance(time.Second)
		res, err := src.Apply(ctx, cs)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range res.Subjects {
			ids = append(ids, string(id))
		}
	}
	type journalEdit = func([]*modelv1alpha1.JournalEntry) []*modelv1alpha1.JournalEntry
	for name, tamper := range map[string]journalEdit{
		"recorded_at going back": func(j []*modelv1alpha1.JournalEntry) []*modelv1alpha1.JournalEntry {
			retime(j[2], j[1].GetChangeSet().GetRecordedAt().AsTime())
			return j
		},
		"no recorded_at": func(j []*modelv1alpha1.JournalEntry) []*modelv1alpha1.JournalEntry {
			j[0].ChangeSet.RecordedAt = nil
			return j
		},
		"recorded_at in the future": func(j []*modelv1alpha1.JournalEntry) []*modelv1alpha1.JournalEntry {
			retime(j[2], clk.Now().Add(time.Second))
			return j
		},
		"an event twice": func(j []*modelv1alpha1.JournalEntry) []*modelv1alpha1.JournalEntry {
			again := proto.CloneOf(j[0])
			retime(again, j[0].GetChangeSet().GetRecordedAt().AsTime().Add(time.Millisecond))
			return append([]*modelv1alpha1.JournalEntry{j[0], again}, j[1:]...)
		},
		"an outcome it doesn't get": func(j []*modelv1alpha1.JournalEntry) []*modelv1alpha1.JournalEntry {
			j[2].Minted[0].Kind = "Person"
			return j
		},
		"a record over the size limit": func(j []*modelv1alpha1.JournalEntry) []*modelv1alpha1.JournalEntry {
			j[2].ChangeSet.EventId = strings.Repeat("e", contracts.MaxChangeSetBytes)
			return j
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := tamper(journal(t, src))
			dst, dclk := newTestStore()
			dclk.Set(clk.Now())
			if err := dst.Restore(ctx, backupOf(t, j, clk.Now())); err == nil {
				t.Fatal("got no error, want one")
			}
			if head, _ := dst.Head(ctx); !head.IsZero() {
				t.Fatalf("got head %s, want an empty store", head)
			}
			for _, id := range ids {
				if _, err := dst.Subject(ctx, contracts.SubjectID(id), time.Time{}); !errors.Is(err, contracts.ErrNotFound) {
					t.Fatalf("got %v, want %s gone", err, id)
				}
			}
		})
	}
	// A backup that doesn't say when it was taken restores nothing.
	if empty, _ := newTestStore(); empty.Restore(ctx, backupOf(t, journal(t, src), time.Time{})) == nil {
		t.Fatal("restored a backup with no taken_at")
	}
	// Untampered, the same journal restores.
	dst, dclk := newTestStore()
	dclk.Set(clk.Now())
	if err := dst.Restore(ctx, backupOf(t, journal(t, src), clk.Now())); err != nil {
		t.Fatal(err)
	}
}
