package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
)

// newCheckpointWorld is a retention world whose server also writes checkpoints
// of the audit log's head to the world's file, signed with its key when signed.
func newCheckpointWorld(t *testing.T, signed bool) *retentionWorld {
	t.Helper()
	w := newRetentionWorld(t)
	w.cfg.AuditRetention = AuditRetention{}
	w.cfg.AuditCheckpoints = AuditCheckpoints{Enabled: true, Checkpoints: w.dest}
	if signed {
		w.cfg.AuditCheckpoints.Signer = func() (*audit.Signer, error) { return w.keyFn() }
	}
	var err error
	if w.s, err = New(w.cfg); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *retentionWorld) checkpoints(t *testing.T) []*modelv1alpha1.AuditCheckpoint {
	t.Helper()
	cps, err := w.file.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return cps
}

func TestAuditCheckpointsPinTheHeadSignedAndOnlyWhenItMoves(t *testing.T) {
	t.Parallel()
	w := newCheckpointWorld(t, true)
	var last contracts.AuditHead
	w.write(t, 3)
	w.s.auditCheckpointRun(t.Context(), &last)
	w.s.auditCheckpointRun(t.Context(), &last)
	cps := w.checkpoints(t)
	if len(cps) != 1 || cps[0].GetSeq() != 3 || cps[0].GetCut() || cps[0].GetKeyId() != "audit-1" {
		t.Fatalf("got %v, want one signed, uncut checkpoint of record 3 and none for the unchanged head", cps)
	}
	if rep := w.verifies(t); !rep.OK() || rep.Checkpoints != 1 {
		t.Fatalf("got %+v, want the checkpoint to verify against the log", rep)
	}
	w.write(t, 2)
	w.s.auditCheckpointRun(t.Context(), &last)
	if cps := w.checkpoints(t); len(cps) != 2 || cps[1].GetSeq() != 5 {
		t.Fatalf("got %v, want a second checkpoint of record 5 once the head moved", cps)
	}
}

func TestAuditCheckpointsWriteUnsignedOnlyWhenNoKeyIsConfigured(t *testing.T) {
	t.Parallel()
	w := newCheckpointWorld(t, false)
	w.write(t, 2)
	var last contracts.AuditHead
	w.s.auditCheckpointRun(t.Context(), &last)
	cps := w.checkpoints(t)
	if len(cps) != 1 || cps[0].GetKeyId() != "" || len(cps[0].GetSignature()) != 0 {
		t.Fatalf("got %v, want one unsigned checkpoint", cps)
	}
	// Verify refuses it unless the operator allows it.
	opts := audit.Options{Keys: map[string]ed25519.PublicKey{"audit-1": w.pub}}
	if rep, err := audit.Verify(t.Context(), w.mem.AuditLog(), cps, opts); err != nil || rep.OK() {
		t.Fatalf("got %+v, %v, want verify to refuse an unsigned checkpoint", rep, err)
	}
	opts.AllowUnsigned = true
	if rep, err := audit.Verify(t.Context(), w.mem.AuditLog(), cps, opts); err != nil || !rep.OK() {
		t.Fatalf("got %+v, %v, want the unsigned checkpoint to pass when allowed", rep, err)
	}
}

func TestAuditCheckpointsNeverFallBackToUnsignedWhenTheKeyStopsLoading(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]func() (*audit.Signer, error){
		"an error":      func() (*audit.Signer, error) { return nil, errors.New("key file is gone") },
		"no signer":     func() (*audit.Signer, error) { return nil, nil },
		"a bad key ID":  func() (*audit.Signer, error) { return &audit.Signer{KeyID: "a b", Key: nil}, nil },
		"a short key":   func() (*audit.Signer, error) { return &audit.Signer{KeyID: "k", Key: []byte{1}}, nil },
		"a nil key too": func() (*audit.Signer, error) { return &audit.Signer{KeyID: "k"}, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newCheckpointWorld(t, true)
			w.write(t, 3)
			w.keyFn = key
			var last contracts.AuditHead
			w.s.auditCheckpointRun(t.Context(), &last)
			if n := len(w.checkpoints(t)); n != 0 || w.dest.appended != 0 {
				t.Fatalf("got %d checkpoints, want none once the key stops loading", n)
			}
			// The key coming back resumes the writer.
			w.keyFn = func() (*audit.Signer, error) { return &audit.Signer{KeyID: "audit-1", Key: w.priv}, nil }
			w.s.auditCheckpointRun(t.Context(), &last)
			if cps := w.checkpoints(t); len(cps) != 1 || cps[0].GetKeyId() != "audit-1" {
				t.Fatalf("got %v, want a signed checkpoint once the key loads again", cps)
			}
		})
	}
}

func TestAuditCheckpointsTryAgainAfterTheDestinationFails(t *testing.T) {
	t.Parallel()
	w := newCheckpointWorld(t, true)
	w.write(t, 3)
	var last contracts.AuditHead
	w.dest.appendErr = errors.New("disk full")
	w.s.auditCheckpointRun(t.Context(), &last)
	if len(w.checkpoints(t)) != 0 || last.Seq != 0 {
		t.Fatalf("got %d checkpoints and last head %d, want nothing written and nothing remembered", len(w.checkpoints(t)), last.Seq)
	}
	w.dest.appendErr = nil
	w.s.auditCheckpointRun(t.Context(), &last)
	if cps := w.checkpoints(t); len(cps) != 1 || cps[0].GetSeq() != 3 {
		t.Fatalf("got %v, want the checkpoint written once the destination works", cps)
	}
}

// wrongHead reports a head that no record carries.
type wrongHead struct{ contracts.AuditLog }

func (w wrongHead) Head(ctx context.Context) (contracts.AuditHead, error) {
	h, err := w.AuditLog.Head(ctx)
	h.Hash = append([]byte(nil), h.Hash...)
	h.Hash[0] ^= 0xff
	return h, err
}

func TestAuditCheckpointsRefuseAHeadThatNoRecordCarries(t *testing.T) {
	t.Parallel()
	for name, log := range map[string]func(contracts.AuditLog) contracts.AuditLog{
		"a head hash the newest record lacks": func(l contracts.AuditLog) contracts.AuditLog { return wrongHead{l} },
		"a head that cannot be read": func(l contracts.AuditLog) contracts.AuditLog {
			return &flakyAudit{AuditLog: l, headErr: errors.New("down")}
		},
		"a record that cannot be read": func(l contracts.AuditLog) contracts.AuditLog { return &flakyAudit{AuditLog: l, failFrom: 1} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newCheckpointWorld(t, true)
			w.write(t, 3)
			w.cfg.Audit = log(w.mem.AuditLog())
			var err error
			if w.s, err = New(w.cfg); err != nil {
				t.Fatal(err)
			}
			var last contracts.AuditHead
			w.s.auditCheckpointRun(t.Context(), &last)
			if n := len(w.checkpoints(t)); n != 0 {
				t.Fatalf("got %d checkpoints, want none for %s", n, name)
			}
		})
	}
}

func TestAuditCheckpointsLeaveAnEmptyLogAlone(t *testing.T) {
	t.Parallel()
	w := newCheckpointWorld(t, true)
	var last contracts.AuditHead
	w.s.auditCheckpointRun(t.Context(), &last)
	if n := len(w.checkpoints(t)); n != 0 {
		t.Fatalf("got %d checkpoints, want none for an empty log", n)
	}
}

func TestAuditCheckpointsNeedADestinationAndASaneInterval(t *testing.T) {
	t.Parallel()
	w := newCheckpointWorld(t, true)
	good := w.cfg.AuditCheckpoints
	for name, tt := range map[string]struct {
		mod  func(*AuditCheckpoints)
		want string
	}{
		"no destination":              {func(a *AuditCheckpoints) { a.Checkpoints = nil }, "destination outside the store"},
		"an interval under the floor": {func(a *AuditCheckpoints) { a.Every = time.Second }, "floor"},
		"a key that does not load": {func(a *AuditCheckpoints) {
			a.Signer = func() (*audit.Signer, error) { return nil, errors.New("no file") }
		}, "does not load"},
		"no signer returned": {func(a *AuditCheckpoints) { a.Signer = func() (*audit.Signer, error) { return nil, nil } }, "none was returned"},
		"a key that cannot sign": {func(a *AuditCheckpoints) {
			a.Signer = func() (*audit.Signer, error) { return &audit.Signer{KeyID: "k"}, nil }
		}, "cannot sign"},
		"a negative interval": {func(a *AuditCheckpoints) { a.Every = -time.Hour }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := w.cfg
			cfg.AuditCheckpoints = good
			tt.mod(&cfg.AuditCheckpoints)
			s, err := New(cfg)
			if tt.want == "" {
				// A zero or negative interval takes the default.
				if err != nil || s.cfg.AuditCheckpoints.Every != DefaultCheckpointEvery {
					t.Fatalf("got %v, %v, want the default interval of %s", s, err, DefaultCheckpointEvery)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
	// No audit log to read.
	cfg := w.cfg
	cfg.Audit = nil
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "audit log") {
		t.Fatalf("got %v, want an error about the audit log", err)
	}
	// Off by default: nothing needed, nothing runs.
	off := w.cfg
	off.AuditCheckpoints = AuditCheckpoints{}
	s, err := New(off)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.runAuditCheckpoints(t.Context()); err != nil {
		t.Fatal(err)
	}
	// No signer at all is allowed: the checkpoints are unsigned.
	unsigned := w.cfg
	unsigned.AuditCheckpoints.Signer = nil
	if _, err := New(unsigned); err != nil {
		t.Fatalf("got %v, want a server with unsigned checkpoints to start", err)
	}
}

func TestAuditCheckpointsRunAtStartAndOnTheirTimerInARunningServer(t *testing.T) {
	t.Parallel()
	w := newCheckpointWorld(t, true)
	w.cfg.AuditCheckpoints.Every = time.Minute
	w.cfg.LockCheck, w.cfg.PollInterval = 24*time.Hour, 24*time.Hour
	var err error
	if w.s, err = New(w.cfg); err != nil {
		t.Fatal(err)
	}
	w.write(t, 3)
	w.start()
	waitFor(t, "the checkpoint at start", func() bool { return len(w.checkpoints(t)) == 1 })
	// More records, then ticks until the writer has seen them: the first
	// advance may come before the writer's ticker exists, and a tick during
	// the writes may pin record 4 on the way.
	w.write(t, 2)
	waitFor(t, "the checkpoint of the newest record after an interval", func() bool {
		w.clk.Advance(time.Minute)
		cps := w.checkpoints(t)
		return len(cps) >= 2 && cps[len(cps)-1].GetSeq() == 5
	})
	if rep := w.verifies(t); !rep.OK() {
		t.Fatalf("got %+v, want every checkpoint to verify", rep.Failures)
	}
}

func TestAuditCheckpointsAndRetentionShareOneFileThatStillVerifies(t *testing.T) {
	t.Parallel()
	w := newRetentionWorld(t)
	w.cfg.AuditCheckpoints = AuditCheckpoints{Enabled: true, Checkpoints: w.dest, Signer: func() (*audit.Signer, error) { return w.keyFn() }}
	var err error
	if w.s, err = New(w.cfg); err != nil {
		t.Fatal(err)
	}
	var last contracts.AuditHead
	w.write(t, 10)
	w.s.auditCheckpointRun(t.Context(), &last)
	w.clk.Advance(20*time.Hour + 30*time.Minute) // records 1 to 6 are over a day old
	w.s.auditRetentionRun(t.Context())
	w.s.auditCheckpointRun(t.Context(), &last)
	if got := w.oldestSeq(t); got != 7 {
		t.Fatalf("got the oldest record %d, want 7", got)
	}
	// One checkpoint of record 10, one cut marker, and nothing new for a head
	// the cut did not move.
	if cps := w.checkpoints(t); len(cps) != 2 || cps[0].GetCut() || !cps[1].GetCut() {
		t.Fatalf("got %v, want a checkpoint and then a cut marker", cps)
	}
	if rep := w.verifies(t); !rep.OK() || rep.Cut == nil {
		t.Fatalf("got %+v, want the cut log to verify against both kinds of checkpoint", rep)
	}
}
