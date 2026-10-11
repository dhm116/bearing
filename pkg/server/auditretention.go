package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/telemetry"
)

// Defaults and bounds of AuditRetention.
const (
	// DefaultAuditAge is how old a record must be to be cut when retention
	// is enabled and gives no age.
	DefaultAuditAge = 365 * 24 * time.Hour
	// MinAuditAge is the youngest age retention accepts: a record is kept at
	// least a day, so a misconfigured age can't wipe the recent past.
	MinAuditAge = 24 * time.Hour
	// DefaultAuditEvery is how often the job looks for records to cut.
	DefaultAuditEvery = time.Hour
	// DefaultAuditBatch is how many records one Cut removes; the head lock
	// is held for the delete, so a cut stays small.
	DefaultAuditBatch = 1000
	// DefaultAuditMaxCuts bounds the cuts of one run.
	DefaultAuditMaxCuts = 100
)

// CheckpointLog is where the retention job keeps its cut checkpoints and
// reads the others: the place outside the store that the operator keeps where
// the store's writers can't change it (threat model C-AUDIT-3). audit.CheckpointFile
// is the file implementation.
type CheckpointLog interface {
	// Append writes cp and returns only when it is durable (synced, or
	// acknowledged by a remote destination). The job cuts nothing until it
	// has returned.
	Append(ctx context.Context, cp *modelv1alpha1.AuditCheckpoint) error
	// Read returns every checkpoint written so far.
	Read(ctx context.Context) ([]*modelv1alpha1.AuditCheckpoint, error)
}

// AuditRetention configures the job that cuts the oldest audit records
// (docs/spec/contracts.md, "Audit retention"). It is off unless Enabled, and
// it needs a signing key: an unsigned marker never counts, so without a key
// New refuses to start, and a key that stops loading stops the job.
type AuditRetention struct {
	// Enabled turns the job on.
	Enabled bool
	// Age is how old a record must be to be cut; zero means DefaultAuditAge,
	// and anything under MinAuditAge is refused.
	Age time.Duration
	// Every is how often the job runs; Batch is how many records one cut
	// removes and MaxCuts how many cuts one run makes.
	Every   time.Duration
	Batch   int
	MaxCuts int
	// Cut removes the records. Only this job holds it.
	Cut contracts.AuditRetention
	// Checkpoints is the destination of the cut checkpoints, and the file the
	// job verifies the log against before it signs one.
	Checkpoints CheckpointLog
	// Signer returns the signing key, read each time the job runs so a key
	// that is rotated or stops loading is noticed. It never falls back to an
	// unsigned marker.
	Signer func() (*audit.Signer, error)
	// Keys are the public keys the job verifies the log's checkpoints with,
	// by key ID: those of every key that signed one still in the file. The
	// public half of the current signing key is added.
	Keys map[string]ed25519.PublicKey
}

// checkAuditRetention validates c, with defaults filled in, and loads the key
// once so a server with no usable key does not start.
func checkAuditRetention(cfg *Config) error {
	c := &cfg.AuditRetention
	if !c.Enabled {
		return nil
	}
	switch {
	case cfg.Audit == nil:
		return errors.New("server: audit retention needs the audit log")
	case c.Cut == nil:
		return errors.New("server: audit retention needs the retention cut")
	case c.Checkpoints == nil:
		return errors.New("server: audit retention needs a checkpoint destination outside the store")
	case c.Signer == nil:
		return errors.New("server: audit retention needs a signing key: an unsigned marker never counts")
	}
	if c.Age == 0 {
		c.Age = DefaultAuditAge
	}
	if c.Age < MinAuditAge {
		return fmt.Errorf("server: audit retention age %s is under the floor of %s", c.Age, MinAuditAge)
	}
	c.Every = orDefault(c.Every, DefaultAuditEvery)
	c.Batch = min(orDefault(c.Batch, DefaultAuditBatch), contracts.MaxAuditQueryRecords)
	c.MaxCuts = orDefault(c.MaxCuts, DefaultAuditMaxCuts)
	if _, err := c.Signer(); err != nil {
		return fmt.Errorf("server: audit retention: the signing key does not load: %w", err)
	}
	return nil
}

// runAuditRetention cuts every Every. A run that fails is reported and tried
// again at the next tick; none of its failures stops the server.
func (s *Server) runAuditRetention(ctx context.Context) error {
	if !s.cfg.AuditRetention.Enabled {
		return nil
	}
	t := s.clk.NewTicker(s.cfg.AuditRetention.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
		}
		s.auditRetentionRun(ctx)
	}
}

// auditRetentionRun makes the cuts of one run, until nothing is old enough or
// MaxCuts is reached. The log is verified once, before the first marker is
// signed: it is a read of the whole log, and the cuts that follow take their
// hashes from the same chain.
func (s *Server) auditRetentionRun(ctx context.Context) {
	ctx, span := tracer.Start(ctx, "retention.audit_cut")
	defer span.End()
	verified := false
	for range s.cfg.AuditRetention.MaxCuts {
		more, stage, err := s.auditCut(ctx, &verified)
		if err != nil {
			if ctx.Err() == nil {
				auditCuts.Add(ctx, 1, resultStageAttrs("failed", stage))
				_ = telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "cutting the audit log failed", errors.New(cleanErrorText(err.Error())), attribute.String("bearing.stage", stage))
			}
			return
		}
		if !more {
			return
		}
		auditCuts.Add(ctx, 1, resultStageAttrs("cut", ""))
	}
}

// auditCut makes one cut and reports whether it did. This is the one place
// the order lives: choose what to cut (an unfinished cut first, else the
// records past the age), verify the log against the checkpoints, write the
// signed marker durably, then cut. A failure at any step stops before the
// next, so a failing destination cuts nothing, a failing verify signs and
// cuts nothing, and a failing cut leaves a marker that the next run finishes
// before it chooses anything else. stage names the step that failed.
func (s *Server) auditCut(ctx context.Context, verified *bool) (cut bool, stage string, err error) {
	c := s.cfg.AuditRetention
	now := s.clk.Now().UTC()
	signer, err := c.Signer()
	if err != nil {
		return false, "key", fmt.Errorf("the signing key does not load: %w", err)
	}
	keys := map[string]ed25519.PublicKey{}
	for id, k := range c.Keys {
		keys[id] = k
	}
	if pub, ok := signer.Key.Public().(ed25519.PublicKey); ok {
		keys[signer.KeyID] = pub
	}
	cps, err := c.Checkpoints.Read(ctx)
	if err != nil {
		return false, "read", fmt.Errorf("read the checkpoints: %w", err)
	}
	head, err := s.cfg.Audit.Head(ctx)
	if err != nil {
		return false, "choose", fmt.Errorf("read the head: %w", err)
	}
	oldest, err := s.cfg.Audit.Query(ctx, contracts.AuditFilter{Limit: 1})
	if err != nil {
		return false, "choose", fmt.Errorf("read the oldest record: %w", err)
	}
	if head.Seq == 0 || len(oldest) == 0 {
		return false, "", nil
	}
	through, pending, err := s.chooseCut(ctx, cps, keys, oldest[0].GetSeq(), head.Seq, now.Add(-c.Age))
	if err != nil {
		return false, "choose", err
	}
	if through == nil {
		return false, "", nil
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.Int64("bearing.audit.cut.through", int64(through.Seq)), attribute.Bool("bearing.audit.cut.finishing", pending)) //nolint:gosec // G115: a sequence number never reaches 2^63
	if !*verified {
		rep, err := audit.Verify(ctx, s.cfg.Audit, cps, audit.Options{Keys: keys})
		if err != nil {
			return false, "verify", err
		}
		if !rep.OK() {
			return false, "verify", fmt.Errorf("the log or its checkpoints do not verify, so nothing is signed or cut: %s", rep.Failures[0].Error())
		}
		*verified = true
	}
	if !pending {
		marker, err := audit.NewCutCheckpoint(*through, now, signer)
		if err != nil {
			return false, "marker", err
		}
		if err := c.Checkpoints.Append(ctx, marker); err != nil {
			return false, "marker", fmt.Errorf("write the cut checkpoint: %w", err)
		}
	}
	if _, err := c.Cut.Cut(ctx, contracts.AuditCut{Through: through.Seq, Hash: through.Hash}); err != nil {
		return false, "cut", err
	}
	return true, "", nil
}

// chooseCut picks the last record to remove: the record of an unfinished cut,
// else the newest record in the batch of records older than cutoff. An
// unfinished cut is a trusted cut checkpoint for a record the log still holds
// that is not its newest. It never picks the newest record. Nil means there
// is nothing to cut.
func (s *Server) chooseCut(ctx context.Context, cps []*modelv1alpha1.AuditCheckpoint, keys map[string]ed25519.PublicKey, oldest, head uint64, cutoff time.Time) (through *contracts.AuditHead, pending bool, err error) {
	var best *modelv1alpha1.AuditCheckpoint
	for _, cp := range cps {
		if !cp.GetCut() || cp.GetSeq() < oldest || cp.GetSeq() >= head || len(cp.GetSignature()) == 0 || audit.VerifyCheckpoint(cp, keys) != nil {
			continue
		}
		if best == nil || cp.GetSeq() > best.GetSeq() {
			best = cp
		}
	}
	if best != nil {
		return &contracts.AuditHead{Seq: best.GetSeq(), Hash: best.GetHeadHash()}, true, nil
	}
	recs, err := s.cfg.Audit.Query(ctx, contracts.AuditFilter{Limit: s.cfg.AuditRetention.Batch, To: cutoff})
	if err != nil {
		return nil, false, fmt.Errorf("read the records past the age: %w", err)
	}
	for len(recs) > 0 && recs[len(recs)-1].GetSeq() >= head {
		recs = recs[:len(recs)-1]
	}
	if len(recs) == 0 {
		return nil, false, nil
	}
	last := recs[len(recs)-1]
	return &contracts.AuditHead{Seq: last.GetSeq(), Hash: last.GetHash(), RecordedAt: last.GetRecordedAt().AsTime()}, false, nil
}
