package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

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
	// MinCheckpointEvery is the shortest interval accepted: the file is read
	// whole by Verify and the retention job and is bounded
	// (audit.MaxCheckpointFileBytes), so a short interval would fill it.
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
	// unless the operator allows them). A Signer that returns an error writes
	// nothing: it never falls back to an unsigned checkpoint.
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
	var last contracts.AuditHead
	s.auditCheckpointRun(ctx, &last)
	t := s.clk.NewTicker(c.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
		}
		s.auditCheckpointRun(ctx, &last)
	}
}

// auditCheckpointRun writes the checkpoint of the log's head unless last, the
// head it last checkpointed, is still the head. It signs only when a key is
// configured, and a configured key that does not load writes nothing.
func (s *Server) auditCheckpointRun(ctx context.Context, last *contracts.AuditHead) {
	ctx, span := tracer.Start(ctx, "audit.checkpoint")
	defer span.End()
	result, stage, err := s.auditCheckpoint(ctx, last)
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
// that does not load writes nothing even when the head has not moved, then the
// head (checked against its record), then the durable append. result is
// "written" or "unchanged"; stage names the step that failed.
func (s *Server) auditCheckpoint(ctx context.Context, last *contracts.AuditHead) (result, stage string, err error) {
	c := s.cfg.AuditCheckpoints
	now := s.clk.Now().UTC()
	var signer *audit.Signer
	if c.Signer != nil {
		if signer, err = loadAuditSigner(c.Signer, now); err != nil {
			return "", "key", err
		}
	}
	head, err := audit.CheckedHead(ctx, s.cfg.Audit)
	if err != nil {
		return "", "head", err
	}
	if head.Seq == 0 || (head.Seq == last.Seq && bytes.Equal(head.Hash, last.Hash)) {
		return "unchanged", "", nil
	}
	cp, err := audit.NewCheckpoint(head, now, signer)
	if err != nil {
		return "", "checkpoint", err
	}
	if err := c.Checkpoints.Append(ctx, cp); err != nil {
		return "", "write", fmt.Errorf("write the checkpoint: %w", err)
	}
	*last = head
	return "written", "", nil
}
