package github

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/telemetry"
)

const pkgName = "adapters/github"

var (
	tracer = telemetry.Tracer(pkgName)
	meter  = telemetry.Meter(pkgName)
)

const (
	attrPhase     = attribute.Key("bearing.github.sync.phase")
	attrPage      = attribute.Key("bearing.github.sync.page")
	attrOrg       = attribute.Key("bearing.github.org")
	attrEvent     = attribute.Key("bearing.github.webhook.event")
	attrAction    = attribute.Key("bearing.github.webhook.action")
	attrResult    = attribute.Key("bearing.result")
	attrRateLimit = attribute.Key("bearing.github.ratelimit.resource")
)

// Metric instruments. See docs/telemetry.md for the catalog. HTTP request
// counts and latencies come from otelhttp (http.client.request.duration).
var (
	webhooks = must(meter.Int64Counter("bearing.github.webhooks",
		metric.WithDescription("Webhook deliveries by event and result (accepted, ignored, rejected)."),
		metric.WithUnit("{delivery}")))
	codeownersLookups = must(meter.Int64Counter("bearing.github.codeowners.lookups",
		metric.WithDescription("Repositories checked for CODEOWNERS, by result (found, missing)."),
		metric.WithUnit("{repository}")))
	rateLimitRemaining = must(meter.Int64Gauge("bearing.github.ratelimit.remaining",
		metric.WithDescription("GitHub API requests left in the current rate-limit window."),
		metric.WithUnit("{request}")))
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
