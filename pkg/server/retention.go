package server

import (
	"context"

	"bearing.example/pkg/store"
	"bearing.example/pkg/telemetry"
)

// runRetention trims the log every TrimEvery, and keeps the dead-letter gauge
// current. Trim is told the apply group, so it keeps an event until the
// workers have applied it.
func (s *Server) runRetention(ctx context.Context) error {
	s.refreshDeadLetters(ctx)
	t := s.clk.NewTicker(s.cfg.TrimEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C():
		}
		if s.cfg.Retention > 0 {
			s.trim(ctx)
		}
		s.refreshDeadLetters(ctx)
	}
}

func (s *Server) trim(ctx context.Context) {
	ctx, span := tracer.Start(ctx, "retention.trim")
	defer span.End()
	n, err := s.cfg.Log.Trim(ctx, s.clk.Now().Add(-s.cfg.Retention), []string{s.cfg.Group})
	if err != nil {
		if ctx.Err() == nil {
			_ = telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "trimming the event log failed", err)
		}
		return
	}
	retentionTrimmed.Add(ctx, int64(n))
}

// refreshDeadLetters counts the records in core/dead_letter, which all stay
// until someone releases them.
func (s *Server) refreshDeadLetters(ctx context.Context) {
	var after store.Offset
	var n int64
	for {
		entries, err := s.cfg.Log.Read(ctx, deadLetterPartition, after, store.MaxReadEntries)
		if err != nil {
			if ctx.Err() == nil {
				telemetry.Logger(pkgName).WarnContext(ctx, "counting dead letters failed", "error", cleanErrorText(err.Error()))
			}
			return
		}
		if len(entries) == 0 {
			break
		}
		n += int64(len(entries))
		after = entries[len(entries)-1].Offset
	}
	s.deadLetters.Store(n)
}
