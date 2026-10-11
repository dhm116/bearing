package server

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/telemetry"
)

// schedulerActor is who a scheduled sync is requested by.
const schedulerActor = "system:scheduler"

// runScheduler asks for a full sync of each source with an interval. It runs
// until ctx ends.
func (s *Server) runScheduler(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, src := range s.cfg.Sources {
		if src.Interval <= 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.scheduleSource(ctx, src)
		}()
	}
	<-ctx.Done()
	return nil
}

func (s *Server) scheduleSource(ctx context.Context, src Source) {
	if src.SyncOnStart {
		s.scheduledSync(ctx, src.Name)
	}
	t := s.clk.NewTicker(src.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			s.scheduledSync(ctx, src.Name)
		}
	}
}

// scheduledSync requests a sync. A source that is slower than its interval
// still has the last request waiting, which RequestSync answers instead of
// piling up another.
func (s *Server) scheduledSync(ctx context.Context, source string) {
	ctx, span := tracer.Start(ctx, "scheduler.sync")
	defer span.End()
	span.SetAttributes(attrSourceName.String(source))
	log := telemetry.Logger(pkgName)
	count := func(result string) {
		schedulerSyncs.Add(ctx, 1, metric.WithAttributes(attrSourceName.String(source), attribute.String(string(attrResult), result)))
	}

	req, err := s.RequestSync(ctx, source, &eventv1alpha1.Actor{Subject: schedulerActor})
	if err != nil {
		if ctx.Err() == nil {
			_ = telemetry.Fail(ctx, span, log, "scheduled sync not requested", err, attrSourceName.String(source))
			count("failed")
		}
		return
	}
	if req.Pending {
		count("skipped")
		return
	}
	count("requested")
}
