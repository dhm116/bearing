package conformance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
)

// RetentionStore is an audit store that also serves the retention cut.
type RetentionStore interface {
	AuditStore
	// AuditRetention returns the store's retention cut.
	AuditRetention() contracts.AuditRetention
}

// AuditRetention runs the AuditRetention conformance suite
// (docs/spec/contracts.md, "Retention"). newStore must return an empty store,
// as for GraphStore.
func AuditRetention(t *testing.T, newStore func(t *testing.T) (RetentionStore, Clock, IDs)) {
	r := &retentionSuite{newStore: newStore}
	for _, c := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"Cut removes the oldest records and the rest still verify behind a signed cut checkpoint", r.prefix},
		{"A repeated or older cut removes nothing and succeeds", r.idempotent},
		{"Cut refuses the newest record, an empty log and a bad request", r.refused},
		{"Cut refuses a record that does not have the hash it was given", r.mismatch},
		{"Applies and cuts together keep one unbroken chain", r.concurrent},
		{"Restore rebuilds the whole chain, since the journal keeps the entries", r.restore},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t)
		})
	}
}

type retentionSuite struct {
	newStore func(t *testing.T) (RetentionStore, Clock, IDs)
}

func (r *retentionSuite) store(t *testing.T) (RetentionStore, Clock) {
	t.Helper()
	s, clk, _ := r.newStore(t)
	clk.Set(at("2026-09-28T01:30:02Z"))
	return s, clk
}

// writeEvents applies n events of one audit entry each, named prefix and a
// number, and a second apart.
func writeEvents(t *testing.T, s AuditStore, clk Clock, prefix string, n int) {
	t.Helper()
	for i := range n {
		clk.Set(clk.Now().Add(time.Second))
		name := fmt.Sprintf("%s%d", prefix, i)
		apply(t, s, &modelv1alpha1.ChangeSet{EventId: name, Audit: []*modelv1alpha1.AuditEntry{entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", "github:repo/acme/"+name)}})
	}
}

// recordAt reads the record numbered seq.
func recordAt(t *testing.T, l contracts.AuditLog, seq uint64) *modelv1alpha1.AuditRecord {
	t.Helper()
	recs, err := l.Query(ctx, contracts.AuditFilter{After: seq - 1, Limit: 1})
	failIf(t, err != nil || len(recs) != 1 || recs[0].GetSeq() != seq, "Query for record %d: got %v, %v", seq, recs, err)
	return recs[0]
}

type cutKey struct {
	signer *audit.Signer
	keys   map[string]ed25519.PublicKey
}

func newCutKey(t *testing.T) cutKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	failIf(t, err != nil, "%v", err)
	return cutKey{&audit.Signer{KeyID: "k1", Key: priv}, map[string]ed25519.PublicKey{"k1": pub}}
}

// cut writes the signed cut checkpoint for record seq, then cuts, as the
// server does, and returns the checkpoint.
func (k cutKey) cut(t *testing.T, s RetentionStore, clk Clock, seq uint64) (*modelv1alpha1.AuditCheckpoint, uint64) {
	t.Helper()
	rec := recordAt(t, s.AuditLog(), seq)
	cp, err := audit.NewCutCheckpoint(contracts.AuditHead{Seq: seq, Hash: rec.GetHash()}, clk.Now(), k.signer)
	failIf(t, err != nil, "%v", err)
	n, err := s.AuditRetention().Cut(ctx, contracts.AuditCut{Through: seq, Hash: rec.GetHash()})
	failIf(t, err != nil, "Cut through %d: %v", seq, err)
	return cp, n
}

func (r *retentionSuite) prefix(t *testing.T) {
	s, clk := r.store(t)
	l := s.AuditLog()
	k := newCutKey(t)
	writeEvents(t, s, clk, "e", 6)
	before := records(t, l)
	head, err := l.Head(ctx)
	failIf(t, err != nil || head.Seq != 6, "Head: got %+v, %v, want record 6", head, err)

	// The oldest record alone is a cut like any other.
	_, n := k.cut(t, s, clk, 1)
	failIf(t, n != 1, "Cut through the oldest record removed %d records, want 1", n)
	cp, n := k.cut(t, s, clk, 3)
	failIf(t, n != 2, "Cut removed %d records, want 2", n)
	got := records(t, l)
	failIf(t, len(got) != 3 || got[0].GetSeq() != 4, "got %d records, want 3 from record 4", len(got))
	for i, rec := range got {
		failIf(t, rec.GetHash() == nil || !bytes.Equal(rec.GetHash(), before[i+3].GetHash()), "record %d was changed by the cut", rec.GetSeq())
	}
	failIf(t, !bytes.Equal(got[0].GetPrevHash(), before[2].GetHash()), "record 4 does not hold record 3's hash")
	h, err := l.Head(ctx)
	failIf(t, err != nil || h.Seq != 6 || !bytes.Equal(h.Hash, head.Hash), "Head after the cut: got %+v, %v, want %+v", h, err, head)
	// Filters see only what is left.
	gone, err := l.Query(ctx, contracts.AuditFilter{Limit: 10, EventID: "e0"})
	failIf(t, err != nil || len(gone) != 0, "Query for a removed event: got %v, %v, want none", gone, err)
	kept, err := l.Query(ctx, contracts.AuditFilter{Limit: 10, EventID: "e4"})
	failIf(t, err != nil || len(kept) != 1, "Query for a kept event: got %v, %v, want one record", kept, err)

	// The cut log verifies behind its signed marker, and not without it.
	rep, err := audit.Verify(ctx, l, []*modelv1alpha1.AuditCheckpoint{cp}, audit.Options{Keys: k.keys})
	failIf(t, err != nil || !rep.OK() || rep.First != 4 || rep.Records != 3 || rep.Cut == nil, "Verify with the marker: got %+v, %v, want records 4 to 6 intact", rep, err)
	rep, err = audit.Verify(ctx, l, nil, audit.Options{Keys: k.keys})
	failIf(t, err != nil || rep.OK(), "Verify without the marker: got %+v, %v, want a failure", rep, err)

	// Records written after the cut chain from the head.
	writeEvents(t, s, clk, "f", 2)
	rep, err = audit.Verify(ctx, l, []*modelv1alpha1.AuditCheckpoint{cp}, audit.Options{Keys: k.keys})
	failIf(t, err != nil || !rep.OK() || rep.Last != 8 || rep.Records != 5, "Verify after more applies: got %+v, %v, want records 4 to 8 intact", rep, err)

	// A second cut is another marker; the first is history.
	_, n = k.cut(t, s, clk, 4)
	failIf(t, n != 1, "Cut through the oldest record removed %d records, want 1", n)
	cp2, n := k.cut(t, s, clk, 6)
	failIf(t, n != 2, "second Cut removed %d records, want 2", n)
	rep, err = audit.Verify(ctx, l, []*modelv1alpha1.AuditCheckpoint{cp, cp2}, audit.Options{Keys: k.keys})
	failIf(t, err != nil || !rep.OK() || rep.First != 7 || rep.Records != 2, "Verify after two cuts: got %+v, %v, want records 7 and 8 intact", rep, err)
}

func (r *retentionSuite) idempotent(t *testing.T) {
	s, clk := r.store(t)
	k := newCutKey(t)
	writeEvents(t, s, clk, "e", 5)
	k.cut(t, s, clk, 3)
	// The same cut again, as after a crash between the marker and the reply, and
	// a cut below the oldest record the log holds.
	for _, through := range []uint64{3, 2, 1} {
		n, err := s.AuditRetention().Cut(ctx, contracts.AuditCut{Through: through, Hash: bytes.Repeat([]byte{7}, 32)})
		failIf(t, err != nil || n != 0, "Cut through %d again: got %d, %v, want nothing removed and no error", through, n, err)
	}
	failIf(t, len(records(t, s.AuditLog())) != 2, "got %d records, want the 2 that were left", len(records(t, s.AuditLog())))
}

func (r *retentionSuite) refused(t *testing.T) {
	s, clk := r.store(t)
	rt := s.AuditRetention()
	good := bytes.Repeat([]byte{1}, 32)
	_, err := rt.Cut(ctx, contracts.AuditCut{Through: 1, Hash: good})
	failIf(t, !errors.Is(err, contracts.ErrInvalidAuditCut), "Cut of an empty log: got %v, want ErrInvalidAuditCut", err)
	writeEvents(t, s, clk, "e", 3)
	l := s.AuditLog()
	newest := recordAt(t, l, 3)
	for name, c := range map[string]contracts.AuditCut{
		"the newest record":            {Through: 3, Hash: newest.GetHash()},
		"past the newest record":       {Through: 4, Hash: newest.GetHash()},
		"far past the newest record":   {Through: 1 << 62, Hash: newest.GetHash()},
		"record zero":                  {Through: 0, Hash: newest.GetHash()},
		"no hash":                      {Through: 1},
		"a hash that is not a SHA-256": {Through: 1, Hash: []byte{1, 2, 3}},
	} {
		n, err := rt.Cut(ctx, c)
		failIf(t, !errors.Is(err, contracts.ErrInvalidAuditCut) || n != 0, "Cut through %s: got %d, %v, want ErrInvalidAuditCut", name, n, err)
	}
	failIf(t, len(records(t, l)) != 3, "got %d records after refused cuts, want all 3", len(records(t, l)))
}

func (r *retentionSuite) mismatch(t *testing.T) {
	s, clk := r.store(t)
	l := s.AuditLog()
	writeEvents(t, s, clk, "e", 4)
	other := recordAt(t, l, 2)
	n, err := s.AuditRetention().Cut(ctx, contracts.AuditCut{Through: 3, Hash: other.GetHash()})
	failIf(t, !errors.Is(err, contracts.ErrAuditCutMismatch) || n != 0, "Cut with another record's hash: got %d, %v, want ErrAuditCutMismatch", n, err)
	n, err = s.AuditRetention().Cut(ctx, contracts.AuditCut{Through: 3, Hash: bytes.Repeat([]byte{9}, 32)})
	failIf(t, !errors.Is(err, contracts.ErrAuditCutMismatch) || n != 0, "Cut with a made-up hash: got %d, %v, want ErrAuditCutMismatch", n, err)
	failIf(t, len(records(t, l)) != 4, "got %d records after refused cuts, want all 4", len(records(t, l)))
}

// try panics on an error and reportTo turns that panic into an error on errs,
// so a goroutine of a test needs no branch for each call it makes.
func try[T any](v T, err error) T {
	check(err)
	return v
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func reportTo(errs chan<- error) {
	if r := recover(); r != nil {
		errs <- fmt.Errorf("%v", r)
	}
}

func (r *retentionSuite) concurrent(t *testing.T) {
	s, clk := r.store(t)
	k := newCutKey(t)
	writeEvents(t, s, clk, "seed", 12)
	const writers, each = 6, 4
	var wg sync.WaitGroup
	errs := make(chan error, writers+1)
	for w := range writers {
		wg.Go(func() {
			defer reportTo(errs)
			for i := range each {
				cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("w%d-%d", w, i), Audit: []*modelv1alpha1.AuditEntry{
					entry(modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN, "core/resolver", fmt.Sprintf("github:repo/acme/w%d-%d", w, i)),
				}}
				for {
					if head := try(s.Head(ctx)); !head.IsZero() {
						cs.BaseRecordedAt = timestamppb.New(head)
					}
					_, err := s.Apply(ctx, cs)
					if errors.Is(err, contracts.ErrStale) {
						continue
					}
					check(err)
					break
				}
			}
		})
	}
	var last *modelv1alpha1.AuditCheckpoint
	wg.Go(func() {
		defer reportTo(errs)
		l := s.AuditLog()
		for range 6 {
			through := try(l.Head(ctx)).Seq - 4
			if through < try(l.Query(ctx, contracts.AuditFilter{Limit: 1}))[0].GetSeq() {
				continue
			}
			rec := try(l.Query(ctx, contracts.AuditFilter{After: through - 1, Limit: 1}))[0]
			cp := try(audit.NewCutCheckpoint(contracts.AuditHead{Seq: through, Hash: rec.GetHash()}, at("2026-09-28T01:30:02Z"), k.signer))
			try(s.AuditRetention().Cut(ctx, contracts.AuditCut{Through: through, Hash: rec.GetHash()}))
			last = cp
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	failIf(t, last == nil, "no cut ran")
	rep, err := audit.Verify(ctx, s.AuditLog(), []*modelv1alpha1.AuditCheckpoint{last}, audit.Options{Keys: k.keys})
	failIf(t, err != nil || !rep.OK() || rep.First != last.GetSeq()+1 || rep.Last != 12+writers*each, "Verify: got %+v, %v, want records %d to %d intact", rep, err, last.GetSeq()+1, 12+writers*each)
}

func (r *retentionSuite) restore(t *testing.T) {
	s, clk := r.store(t)
	k := newCutKey(t)
	writeEvents(t, s, clk, "e", 6)
	k.cut(t, s, clk, 4)
	var buf bytes.Buffer
	failIf(t, s.Backup(ctx, &buf) != nil, "Backup failed")
	restored, _ := r.store(t)
	failIf(t, restored.Restore(ctx, bytes.NewReader(buf.Bytes())) != nil, "Restore failed")
	// Restore replays the journal, which still holds the audit entries of the
	// removed records, so the restored log is the whole chain from record 1: a
	// cut bounds the audit table, not the journal and the backups.
	rep, err := audit.Verify(ctx, restored.AuditLog(), nil, audit.Options{AllowUnsigned: true})
	failIf(t, err != nil || !rep.OK() || rep.First != 1 || rep.Records != 6, "Verify of the restored log: got %+v, %v, want records 1 to 6 intact", rep, err)
}
