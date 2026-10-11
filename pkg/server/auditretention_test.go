package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
)

// retentionWorld is a server whose store holds audit records an hour apart,
// a signing key, and a checkpoint file.
type retentionWorld struct {
	*harness
	clk   *testkit.FakeClock
	file  audit.CheckpointFile
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	cut   *cutSpy
	dest  *destSpy
	keyFn func() (*audit.Signer, error)
}

// cutSpy counts the cuts that reach the store and fails them on demand.
type cutSpy struct {
	contracts.AuditRetention
	mu      sync.Mutex
	calls   []contracts.AuditCut
	failing error
}

func (c *cutSpy) Cut(ctx context.Context, cut contracts.AuditCut) (uint64, error) {
	c.mu.Lock()
	c.calls = append(c.calls, cut)
	err := c.failing
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return c.AuditRetention.Cut(ctx, cut)
}

func (c *cutSpy) set(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failing = err
}

func (c *cutSpy) called() []contracts.AuditCut {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]contracts.AuditCut(nil), c.calls...)
}

// destSpy is the checkpoint destination, failing on demand.
type destSpy struct {
	CheckpointLog
	appendErr, readErr error
	appended           int
}

func (d *destSpy) Append(ctx context.Context, cp *modelv1alpha1.AuditCheckpoint) error {
	if d.appendErr != nil {
		return d.appendErr
	}
	d.appended++
	return d.CheckpointLog.Append(ctx, cp)
}

func (d *destSpy) Read(ctx context.Context) ([]*modelv1alpha1.AuditCheckpoint, error) {
	if d.readErr != nil {
		return nil, d.readErr
	}
	return d.CheckpointLog.Read(ctx)
}

// newRetentionWorld builds a server on a fake clock whose retention job cuts
// through a spy; tests then write records with write.
func newRetentionWorld(t *testing.T, mod ...func(*Config)) *retentionWorld {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w := &retentionWorld{clk: testkit.NewClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)), priv: priv, pub: pub}
	w.file = audit.CheckpointFile{Path: t.TempDir() + "/checkpoints.ndjson"}
	w.dest = &destSpy{CheckpointLog: w.file}
	w.keyFn = func() (*audit.Signer, error) { return &audit.Signer{KeyID: "audit-1", Key: priv}, nil }
	w.harness = newHarness(t, func(cfg *Config) {
		cfg.Clock = w.clk
		cfg.AuditRetention = AuditRetention{
			Enabled: true, Age: 24 * time.Hour, Checkpoints: w.dest,
			Signer: func() (*audit.Signer, error) { return w.keyFn() },
			Cut:    &cutSpy{}, // replaced below, once the store exists
		}
		for _, m := range mod {
			m(cfg)
		}
	})
	w.mem.Now = w.clk.Now
	w.cut = &cutSpy{AuditRetention: w.mem.AuditRetention()}
	w.cfg.AuditRetention.Cut = w.cut
	if w.s, err = New(w.cfg); err != nil {
		t.Fatal(err)
	}
	return w
}

// write adds n audit records, an hour apart.
func (w *retentionWorld) write(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		w.clk.Advance(time.Hour)
		cs := &modelv1alpha1.ChangeSet{EventId: fmt.Sprintf("e%d-%d", w.clk.Now().Unix(), i), Audit: []*modelv1alpha1.AuditEntry{{
			Action: modelv1alpha1.AuditAction_AUDIT_ACTION_BINDING_WRITTEN,
			Actor:  &modelv1alpha1.AuditActor{Kind: modelv1alpha1.AuditActorKind_AUDIT_ACTOR_KIND_SYSTEM, Id: "core/resolver"},
			Target: &modelv1alpha1.AuditTarget{Kind: modelv1alpha1.AuditTargetKind_AUDIT_TARGET_KIND_ALIAS, Id: fmt.Sprintf("github:repo/acme/r%d", i)},
		}}}
		if head, err := w.mem.Head(t.Context()); err != nil {
			t.Fatal(err)
		} else if !head.IsZero() {
			cs.BaseRecordedAt = timestamppb.New(head)
		}
		if _, err := w.mem.Apply(t.Context(), cs); err != nil {
			t.Fatal(err)
		}
	}
}

// oldestSeq is the number of the oldest record the log holds.
func (w *retentionWorld) oldestSeq(t *testing.T) uint64 {
	t.Helper()
	recs, err := w.mem.AuditLog().Query(t.Context(), contracts.AuditFilter{Limit: 1})
	if err != nil || len(recs) != 1 {
		t.Fatalf("got %v, %v, want the oldest record", recs, err)
	}
	return recs[0].GetSeq()
}

// verifies checks the log against the checkpoint file with the world's key.
func (w *retentionWorld) verifies(t *testing.T) audit.Report {
	t.Helper()
	cps, err := w.file.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := audit.Verify(t.Context(), w.mem.AuditLog(), cps, audit.Options{Keys: map[string]ed25519.PublicKey{"audit-1": w.pub}})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (w *retentionWorld) markers(t *testing.T) int {
	t.Helper()
	cps, err := w.file.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return len(cps)
}

func TestAuditRetentionCutsTheRecordsPastTheAgeAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t)
	spy := w.cut
	w.write(t, 10)
	w.clk.Advance(20*time.Hour + 30*time.Minute) // records 1 to 6 are over a day old
	w.s.auditRetentionRun(t.Context())
	if got := w.oldestSeq(t); got != 7 {
		t.Fatalf("got the oldest record %d, want 7 (records 1 to 6 are over a day old)", got)
	}
	if rep := w.verifies(t); !rep.OK() || rep.First != 7 || rep.Cut == nil {
		t.Fatalf("got %+v, want the cut log to verify behind the marker in the file", rep)
	}
	if n := w.markers(t); n != 1 {
		t.Fatalf("got %d markers, want 1", n)
	}
	// Nothing more is old enough: a run changes nothing.
	w.s.auditRetentionRun(t.Context())
	if len(spy.called()) != 1 || w.markers(t) != 1 {
		t.Fatalf("got %d cuts and %d markers after a second run, want 1 of each", len(spy.called()), w.markers(t))
	}
	// Far in the future every record is over the age; the newest stays.
	w.clk.Advance(1000 * time.Hour)
	w.s.auditRetentionRun(t.Context())
	head, _ := w.mem.AuditLog().Head(t.Context())
	if got := w.oldestSeq(t); got != head.Seq {
		t.Fatalf("got the oldest record %d, want only the newest, %d", got, head.Seq)
	}
	if rep := w.verifies(t); !rep.OK() || rep.Records != 1 {
		t.Fatalf("got %+v, want one intact record", rep)
	}
}

func TestAuditRetentionCutsInBatchesWithAMarkerForEach(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t, func(cfg *Config) { cfg.AuditRetention.Batch, cfg.AuditRetention.MaxCuts = 2, 3 })
	spy := w.cut
	w.write(t, 12)
	w.clk.Advance(100 * time.Hour)
	w.s.auditRetentionRun(t.Context())
	// Three cuts of two records, one marker each: a run never leaves the log
	// half cut behind a single marker.
	if got := w.oldestSeq(t); got != 7 {
		t.Fatalf("got the oldest record %d, want 7 after 3 cuts of 2", got)
	}
	if len(spy.called()) != 3 || w.markers(t) != 3 {
		t.Fatalf("got %d cuts and %d markers, want 3 of each", len(spy.called()), w.markers(t))
	}
	if rep := w.verifies(t); !rep.OK() || rep.First != 7 {
		t.Fatalf("got %+v, want the log to verify from record 7", rep)
	}
	// The next run carries on.
	w.s.auditRetentionRun(t.Context())
	if got := w.oldestSeq(t); got != 12 {
		t.Fatalf("got the oldest record %d, want 12 (the newest) after the next run", got)
	}
}

func TestAuditRetentionCutsNothingWhenTheCheckpointCannotBeWritten(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t)
	spy := w.cut
	w.write(t, 10)
	w.clk.Advance(100 * time.Hour)
	w.dest.appendErr = errors.New("disk full")
	w.s.auditRetentionRun(t.Context())
	if len(spy.called()) != 0 || w.oldestSeq(t) != 1 {
		t.Fatalf("got %d cuts and the oldest record %d, want nothing cut while the destination fails", len(spy.called()), w.oldestSeq(t))
	}
	// The destination comes back and the job carries on.
	w.dest.appendErr = nil
	w.s.auditRetentionRun(t.Context())
	if w.oldestSeq(t) != 10 {
		t.Fatalf("got the oldest record %d, want 10 once the destination works", w.oldestSeq(t))
	}
}

func TestAuditRetentionSignsAndCutsNothingWhenTheLogDoesNotVerify(t *testing.T) {
	t.Parallel()
	for name, mod := range map[string]func(*retentionWorld, *testing.T){
		"an unsigned checkpoint in the file": func(w *retentionWorld, t *testing.T) {
			head, _ := w.mem.AuditLog().Head(t.Context())
			cp, err := audit.NewCheckpoint(head, w.clk.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.file.Append(t.Context(), cp); err != nil {
				t.Fatal(err)
			}
		},
		"a checkpoint that disagrees with the chain": func(w *retentionWorld, t *testing.T) {
			cp, err := audit.NewCheckpoint(contracts.AuditHead{Seq: 3, Hash: make([]byte, audit.HashSize)}, w.clk.Now(), &audit.Signer{KeyID: "audit-1", Key: w.priv})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.file.Append(t.Context(), cp); err != nil {
				t.Fatal(err)
			}
		},
		"a checkpoint file that cannot be read": func(w *retentionWorld, t *testing.T) {
			w.dest.readErr = errors.New("permission denied")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newRetentionWorld(t)
			spy := &cutSpy{AuditRetention: w.mem.AuditRetention()}
			w.cfg.AuditRetention.Cut = spy
			w.s, _ = New(w.cfg)
			w.write(t, 10)
			w.clk.Advance(100 * time.Hour)
			mod(w, t)
			before := w.dest.appended
			w.s.auditRetentionRun(t.Context())
			if len(spy.called()) != 0 || w.dest.appended != before || w.oldestSeq(t) != 1 {
				t.Fatalf("got %d cuts, %d markers written and the oldest record %d, want nothing signed or cut", len(spy.called()), w.dest.appended-before, w.oldestSeq(t))
			}
		})
	}
}

func TestAuditRetentionFinishesAnUnfinishedCutBeforeChoosingAnother(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t)
	spy := w.cut
	w.write(t, 10)
	w.clk.Advance(20*time.Hour + 30*time.Minute) // records 1 to 6 are over a day old
	spy.set(errors.New("store unavailable"))
	w.s.auditRetentionRun(t.Context())
	first := spy.called()
	if len(first) != 1 || w.markers(t) != 1 || w.oldestSeq(t) != 1 {
		t.Fatalf("got %d cuts, %d markers and the oldest record %d, want one failed cut behind one marker", len(first), w.markers(t), w.oldestSeq(t))
	}
	// Time passes, so more records are old enough, and the store is back, in a
	// new process: the cut the marker licenses is finished first, with the same
	// record, and no second marker is signed for it.
	spy.set(nil)
	w.clk.Advance(2 * time.Hour)
	restarted, err := New(w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	w.s = restarted
	w.s.cfg.AuditRetention.MaxCuts = 1
	w.s.auditRetentionRun(t.Context())
	all := spy.called()
	if len(all) != 2 || all[1].Through != first[0].Through || string(all[1].Hash) != string(first[0].Hash) {
		t.Fatalf("got cuts %+v, want the second to repeat the first", all)
	}
	if w.markers(t) != 1 || w.oldestSeq(t) != first[0].Through+1 {
		t.Fatalf("got %d markers and the oldest record %d, want 1 and %d", w.markers(t), w.oldestSeq(t), first[0].Through+1)
	}
	if rep := w.verifies(t); !rep.OK() {
		t.Fatalf("got %+v, want the log to verify", rep.Failures)
	}
}

func TestAuditRetentionNeverFallsBackToAnUnsignedMarker(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t)
	spy := w.cut
	w.write(t, 10)
	w.clk.Advance(100 * time.Hour)
	w.keyFn = func() (*audit.Signer, error) { return nil, errors.New("key file is gone") }
	w.s.auditRetentionRun(t.Context())
	if len(spy.called()) != 0 || w.markers(t) != 0 || w.oldestSeq(t) != 1 {
		t.Fatalf("got %d cuts and %d markers, want nothing once the key stops loading", len(spy.called()), w.markers(t))
	}
}

func TestAuditRetentionNeedsAKeyADestinationAndASaneAge(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t)
	good := w.cfg.AuditRetention
	good.Cut = w.mem.AuditRetention()
	for name, tt := range map[string]struct {
		mod  func(*AuditRetention)
		want string
	}{
		"no signing key": {func(a *AuditRetention) { a.Signer = nil }, "signing key"},
		"a key that does not load": {func(a *AuditRetention) {
			a.Signer = func() (*audit.Signer, error) { return nil, errors.New("no file") }
		}, "does not load"},
		"no signer returned": {func(a *AuditRetention) { a.Signer = func() (*audit.Signer, error) { return nil, nil } }, "none was returned"},
		"a key that is too short": {func(a *AuditRetention) {
			a.Signer = func() (*audit.Signer, error) { return &audit.Signer{KeyID: "k", Key: []byte{1, 2, 3}}, nil }
		}, "cannot sign"},
		"a key with a bad ID": {func(a *AuditRetention) {
			a.Signer = func() (*audit.Signer, error) { return &audit.Signer{KeyID: "a b", Key: w.priv}, nil }
		}, "cannot sign"},
		"no checkpoint destination": {func(a *AuditRetention) { a.Checkpoints = nil }, "outside the store"},
		"no retention cut":          {func(a *AuditRetention) { a.Cut = nil }, "retention cut"},
		"an age under the floor":    {func(a *AuditRetention) { a.Age = time.Hour }, "floor"},
		"a negative age":            {func(a *AuditRetention) { a.Age = -time.Hour }, "floor"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := w.cfg
			cfg.AuditRetention = good
			tt.mod(&cfg.AuditRetention)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
	// No audit log to read.
	cfg := w.cfg
	cfg.AuditRetention, cfg.Audit = good, nil
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "audit log") {
		t.Fatalf("got %v, want an error about the audit log", err)
	}
	// Off by default: no key needed, nothing runs.
	off := w.cfg
	off.AuditRetention = AuditRetention{}
	s, err := New(off)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.runAuditRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Enabled with no age takes the default.
	def := w.cfg
	def.AuditRetention = good
	def.AuditRetention.Age = 0
	s, err = New(def)
	if err != nil || s.cfg.AuditRetention.Age != DefaultAuditAge {
		t.Fatalf("got %v, %v, want the default age of %s", s, err, DefaultAuditAge)
	}
}

func TestAuditRetentionRunsOnItsTimerInARunningServer(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t, func(cfg *Config) { cfg.Clock = nil; cfg.AuditRetention.Every = 10 * time.Millisecond })
	// The records are dated years ago, so the real clock sees them as old.
	w.mem.Now = func() time.Time { return w.clk.Now().AddDate(-3, 0, 0) }
	w.write(t, 5)
	w.start()
	deadline := time.Now().Add(10 * time.Second)
	for w.oldestSeq(t) != 5 {
		if time.Now().After(deadline) {
			t.Fatalf("got the oldest record %d, want the timer to have cut down to record 5", w.oldestSeq(t))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flakyAudit fails reads of the audit log: Head always when headErr is set,
// and Query from its n-th call on when failFrom is above zero.
type flakyAudit struct {
	contracts.AuditLog
	headErr  error
	failFrom int
	queries  int
}

func (f *flakyAudit) Head(ctx context.Context) (contracts.AuditHead, error) {
	if f.headErr != nil {
		return contracts.AuditHead{}, f.headErr
	}
	return f.AuditLog.Head(ctx)
}

func (f *flakyAudit) Query(ctx context.Context, q contracts.AuditFilter) ([]*modelv1alpha1.AuditRecord, error) {
	f.queries++
	if f.failFrom > 0 && f.queries >= f.failFrom {
		return nil, errors.New("audit log unavailable")
	}
	return f.AuditLog.Query(ctx, q)
}

func TestAuditRetentionDoesNothingWhenItCannotReadTheLog(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]*flakyAudit{
		"the head":                 {headErr: errors.New("audit log unavailable")},
		"the oldest record":        {failFrom: 1},
		"the records past the age": {failFrom: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newRetentionWorld(t)
			f.AuditLog = w.mem.AuditLog()
			w.cfg.Audit = f
			var err error
			if w.s, err = New(w.cfg); err != nil {
				t.Fatal(err)
			}
			w.write(t, 10)
			w.clk.Advance(100 * time.Hour)
			w.s.auditRetentionRun(t.Context())
			if len(w.cut.called()) != 0 || w.markers(t) != 0 || w.oldestSeq(t) != 1 {
				t.Fatalf("got %d cuts and %d markers, want nothing signed or cut while %s cannot be read", len(w.cut.called()), w.markers(t), name)
			}
		})
	}
}

func TestAuditRetentionLeavesAnEmptyLogAlone(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t)
	w.clk.Advance(100 * time.Hour)
	w.s.auditRetentionRun(t.Context())
	if len(w.cut.called()) != 0 || w.markers(t) != 0 {
		t.Fatalf("got %d cuts and %d markers, want nothing from an empty log", len(w.cut.called()), w.markers(t))
	}
}
