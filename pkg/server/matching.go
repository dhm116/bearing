package server

import (
	"context"

	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/match"
	"bearing.example/pkg/telemetry"
)

// matchLinks counts the identity links matching runs write or close.
var matchLinks = must(meter.Int64Counter("bearing.match.links",
	metric.WithDescription("Identity links written or closed by matching runs: asserted (a key rule), proposed (a matcher) or closed (a proposal decayed below the floor)."),
	metric.WithUnit("{link}")))

// runMatching joins records after a sync's last page is applied. A failure is
// reported and never fails the sync: the pages are already in the graph, and
// the next sync matches again.
func (s *Server) runMatching(ctx context.Context, source string) {
	m := s.cfg.Matching
	if m == nil {
		return
	}
	s.matchMu.Lock()
	defer s.matchMu.Unlock()
	ctx, span := tracer.Start(ctx, "match.run")
	defer span.End()
	span.SetAttributes(attrSourceName.String(source))
	log := telemetry.Logger(pkgName)
	cfg := m.Config
	if cfg.Now == nil {
		cfg.Now = s.clk.Now
	}
	res, err := match.Run(ctx, m.Graph, cfg)
	if err != nil {
		_ = telemetry.Fail(ctx, span, log, "matching failed", err, attrSourceName.String(source))
		return
	}
	for result, n := range map[string]int{"asserted": res.Asserted, "proposed": res.Proposed, "closed": res.Closed} {
		matchLinks.Add(ctx, int64(n), metric.WithAttributes(attrResult.String(result)))
	}
	log.InfoContext(ctx, "matching ran after a sync", "bearing.source", source,
		"asserted", res.Asserted, "proposed", res.Proposed, "closed", res.Closed, "for_review", len(res.Review))
}
