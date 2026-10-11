package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/audit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/telemetry"
)

// Defaults and bounds of AuditCheckpoints.
const (
	// DefaultCheckpointEvery is how often the writer looks at the head of the
	// audit log. A checkpoint pins the log up to its head, so this is how much
	// of the newest log an attacker with the store but not the file can still
	// rewrite. A quiet log writes nothing, and a busy one adds a few hundred
	// bytes a quarter hour.
	DefaultCheckpointEvery = 15 * time.Minute
	// MinCheckpointEvery is the shortest interval accepted. The file is read
	// whole by Verify and the retention job and is bounded
	// (audit.MaxCheckpointFileBytes): at the floor a log that changes all the
	// time fills it in about half a year, at the default in about seven years.
	MinCheckpointEvery = time.Minute
)

// AuditCheckpoints configures the writer that checkpoints the head of the
// audit log at an interval (threat model C-AUDIT-3). It is off unless Enabled.
type AuditCheckpoints struct {
	// Enabled turns the writer on.
	Enabled bool
	// Every is the interval; zero means DefaultCheckpointEvery and anything
	// under MinCheckpointEvery is refused.
	Every time.Duration
	// Checkpoints is where the checkpoints go: the place outside the store
	// that the operator keeps where the store's writers can't change it.
	Checkpoints CheckpointLog
	// Signer returns the signing key, read at every write so a rotated key is
	// used and a key that stops loading is noticed. A nil Signer means no key
	// is configured and the checkpoints are unsigned (Verify refuses those
	// unless the operator allows them, and New refuses it while audit
	// retention is on). A Signer that returns an error writes nothing: it
	// never falls back to an unsigned checkpoint. Its key ID must be among
	// AuditRetention.Keys when retention shares the file.
	Signer func() (*audit.Signer, error)
}

// checkAuditCheckpoints validates c, with defaults filled in, and loads the key
// once so a server whose key does not load does not start.
func checkAuditCheckpoints(cfg *Config) error {
	c := &cfg.AuditCheckpoints
	if !c.Enabled {
		return nil
	}
	switch {
	case cfg.Audit == nil:
		return errors.New("server: audit checkpoints need the audit log")
	case c.Checkpoints == nil:
		return errors.New("server: audit checkpoints need a destination outside the store")
	}
	if cfg.AuditRetention.Enabled && c.Signer == nil {
		// Retention verifies the file without AllowUnsigned, so one unsigned
		// line in a file it shares would stop every cut for good.
		return errors.New("server: audit checkpoints need a signing key when audit retention is on, because retention refuses a file with an unsigned line")
	}
	c.Every = orDefault(c.Every, DefaultCheckpointEvery)
	if c.Every < MinCheckpointEvery {
		return fmt.Errorf("server: audit checkpoint interval %s is under the floor of %s", c.Every, MinCheckpointEvery)
	}
	if c.Signer != nil {
		if _, err := loadAuditSigner(c.Signer, cfg.Clock.Now()); err != nil {
			return fmt.Errorf("server: audit checkpoints: %w", err)
		}
	}
	return nil
}

// runAuditCheckpoints writes a checkpoint when it starts and then every Every.
// A run that fails is reported and tried again at the next tick; none of its
// failures stops the server.
func (s *Server) runAuditCheckpoints(ctx context.Context) error {
	c := s.cfg.AuditCheckpoints
	if !c.Enabled {
		return nil
	}
	if c.Signer == nil {
		telemetry.Logger(pkgName).Warn("audit checkpoints are written unsigned, so whoever can write the checkpoint file can forge them; configure a signing key")
	}
	var st checkpointState
	s.auditCheckpointRun(ctx, &st)
	t := s.clk.NewTicker(c.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
		}
		s.auditCheckpointRun(ctx, &st)
	}
}

// checkpointState is what the writer knows between runs: the newest head the
// checkpoint file pins. It is read from the file at the first run, so a
// restart neither repeats a line nor forgets what the file already holds, and
// moves on with every checkpoint the writer appends. A line another process
// appends later (a cut marker, the command line) is not seen until a restart.
// Every line counts, signed or not: a stray line with a high number stops the
// writer at diverged until the operator investigates (see the contracts).
type checkpointState struct {
	pinned contracts.AuditHead
	seeded bool
}

// errDiverged marks a head that does not extend what the file pins.
var errDiverged = errors.New("the log's head does not extend the newest checkpoint in the file")

// auditCheckpointRun writes the checkpoint of the log's head unless st says the
// file already pins it. It signs only when a key is configured, and a
// configured key that does not load writes nothing.
func (s *Server) auditCheckpointRun(ctx context.Context, st *checkpointState) {
	ctx, span := tracer.Start(ctx, "audit.checkpoint")
	defer span.End()
	result, stage, err := s.auditCheckpoint(ctx, st)
	if err != nil {
		if ctx.Err() == nil {
			auditCheckpoints.Add(ctx, 1, resultStageAttrs("failed", stage))
			_ = telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "writing the audit checkpoint failed", errors.New(cleanErrorText(err.Error())), attribute.String("bearing.stage", stage))
		}
		return
	}
	if result != "" {
		auditCheckpoints.Add(ctx, 1, resultStageAttrs(result, ""))
	}
}

// auditCheckpoint is the one place the order lives: the key first, so a key
// that does not load writes nothing even when the head has not moved, then
// what the file already pins, then the head (checked against its record and
// against what the file pins), then the durable append. result is "written" or
// "unchanged"; stage names the step that failed.
func (s *Server) auditCheckpoint(ctx context.Context, st *checkpointState) (result, stage string, err error) {
	c := s.cfg.AuditCheckpoints
	now := s.clk.Now().UTC()
	var signer *audit.Signer
	if c.Signer != nil {
		if signer, err = loadAuditSigner(c.Signer, now); err != nil {
			return "", "key", err
		}
	}
	if !st.seeded {
		cps, err := c.Checkpoints.Read(ctx)
		if err != nil {
			return "", "read", fmt.Errorf("read the checkpoints: %w", err)
		}
		st.pinned, st.seeded = newestPinned(cps), true
	}
	head, err := audit.CheckedHead(ctx, s.cfg.Audit)
	if err != nil {
		return "", "head", err
	}
	// An empty log is checked too: a wiped log under a file that pins records
	// is the extreme case of a head behind the file.
	if err := s.extends(ctx, head, st.pinned); err != nil {
		if errors.Is(err, errDiverged) {
			return "", "diverged", err
		}
		return "", "head", err
	}
	if head.Seq == st.pinned.Seq || head.Seq == 0 {
		return "unchanged", "", nil
	}
	cp, err := audit.NewCheckpoint(head, now, signer)
	if err != nil {
		return "", "checkpoint", err
	}
	if err := c.Checkpoints.Append(ctx, cp); err != nil {
		return "", "write", fmt.Errorf("write the checkpoint: %w", err)
	}
	st.pinned = head
	return "written", "", nil
}

// newestPinned is the head of the checkpoint with the highest sequence number;
// the later line of two for the same number. Cut markers count: they pin a
// record too.
func newestPinned(cps []*modelv1alpha1.AuditCheckpoint) contracts.AuditHead {
	var pinned contracts.AuditHead
	for _, cp := range cps {
		if cp.GetSeq() >= pinned.Seq {
			pinned = contracts.AuditHead{Seq: cp.GetSeq(), Hash: cp.GetHeadHash()}
		}
	}
	return pinned
}

// extends checks that head continues the log that pinned carries: not behind
// it, and either the same record or one that follows the pinned record as it
// was. Someone who rewrote the chain end to end, or cut its tail and mended
// the head, is refused here, where the next checkpoint would have vouched
// for the forgery. A pinned record that retention has cut cannot be compared
// and is skipped; Verify holds the checkpoints that name it to each other.
func (s *Server) extends(ctx context.Context, head, pinned contracts.AuditHead) error {
	switch {
	case pinned.Seq == 0:
		return nil
	case head.Seq < pinned.Seq:
		return fmt.Errorf("%w: the head is record %d and the file pins record %d (a restore, a wiped log, a truncated tail); run bearing audit verify", errDiverged, head.Seq, pinned.Seq)
	case head.Seq == pinned.Seq:
		if !bytes.Equal(head.Hash, pinned.Hash) {
			return fmt.Errorf("%w: record %d has a different hash than the file pins (the chain was rewritten or replaced); run bearing audit verify", errDiverged, head.Seq)
		}
		return nil
	}
	recs, err := s.cfg.Audit.Query(ctx, contracts.AuditFilter{After: pinned.Seq - 1, Limit: 1})
	if err != nil {
		return fmt.Errorf("read the pinned record: %w", err)
	}
	if len(recs) == 1 && recs[0].GetSeq() == pinned.Seq && !bytes.Equal(recs[0].GetHash(), pinned.Hash) {
		return fmt.Errorf("%w: record %d has a different hash than the file pins (the chain was rewritten); run bearing audit verify", errDiverged, pinned.Seq)
	}
	return nil
}
