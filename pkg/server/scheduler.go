package server

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/contracts"
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

// scheduledSync requests a sync unless the one it requested last has not been
// applied yet: a source that is slower than its interval would otherwise pile
// up requests behind itself.
func (s *Server) scheduledSync(ctx context.Context, source string) {
	ctx, span := tracer.Start(ctx, "scheduler.sync")
	defer span.End()
	span.SetAttributes(attrSourceName.String(source))
	log := telemetry.Logger(pkgName)
	count := func(result string) {
		schedulerSyncs.Add(ctx, 1, metric.WithAttributes(attrSourceName.String(source), attribute.String(string(attrResult), result)))
	}

	s.mu.Lock()
	last := s.lastSync[source]
	s.mu.Unlock()
	if last > 0 {
		applied, err := s.cfg.Log.Committed(ctx, s.cfg.Group, contracts.Partition(source))
		if err != nil {
			if ctx.Err() == nil {
				_ = telemetry.Fail(ctx, span, log, "scheduled sync not requested: the log would not say what is applied", err, attrSourceName.String(source))
				count("failed")
			}
			return
		}
		if applied < last {
			count("skipped")
			return
		}
	}
	_, off, err := s.RequestSync(ctx, source, &eventv1alpha1.Actor{Subject: schedulerActor})
	if err != nil {
		if ctx.Err() == nil {
			_ = telemetry.Fail(ctx, span, log, "scheduled sync not requested", err, attrSourceName.String(source))
			count("failed")
		}
		return
	}
	s.mu.Lock()
	s.lastSync[source] = off
	s.mu.Unlock()
	count("requested")
}
