package server

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/telemetry"
)

const pkgName = "pkg/server"

var (
	tracer = telemetry.Tracer(pkgName)
	meter  = telemetry.Meter(pkgName)
)

// Attribute keys shared by the server's spans and metrics.
const (
	attrSourceName = attribute.Key("bearing.source")
	attrResult     = attribute.Key("bearing.result")
)

// deliveries counts webhook deliveries by source and result. The source label
// is a configured source's name or "unknown": a caller cannot choose a value
// for it (C-INGEST-7).
var deliveries = must(meter.Int64Counter("bearing.ingest.deliveries",
	metric.WithDescription("Webhook deliveries by configured source and result: accepted, duplicate, unauthorized, too_large, rate_limited, unavailable or bad_request. A rise in unauthorized for one source is a wrong or leaked key; unavailable means the event log refused and senders will retry."),
	metric.WithUnit("{delivery}")))

func deliveryAttrs(source, result string) metric.AddOption {
	if source == "" {
		source = "unknown"
	}
	return metric.WithAttributes(attrSourceName.String(source), attrResult.String(result))
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
