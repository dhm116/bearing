package server

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/contracts"
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
	metric.WithDescription("Webhook deliveries by configured source and result: accepted, duplicate, unauthorized, too_large, rate_limited, busy (over the limit on deliveries in flight), unavailable or bad_request. A rise in unauthorized for one source is a wrong or leaked key; unavailable means the event log refused and senders will retry."),
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

// Results of handling one event, as labels of bearing.worker.events.
const (
	workerApplied      = "applied"
	workerSkipped      = "skipped"
	workerRetried      = "retried"
	workerDeadLettered = "dead_lettered"
)

// workerEvents counts the outcomes of the apply workers by event type and
// result. A rise in retried is a source or the store failing; dead_lettered
// is an event given up on (docs/spec/contracts.md, "Dead letters").
var workerEvents = must(meter.Int64Counter("bearing.worker.events",
	metric.WithDescription("Events handled by the apply workers by type and result: applied, skipped (a type the server does not handle yet), retried or dead_lettered."),
	metric.WithUnit("{event}")))

var (
	oldestUnappliedGauge = must(meter.Int64ObservableGauge("bearing.worker.oldest_unapplied.age",
		metric.WithDescription("Age of the oldest event in the log that the apply group has not applied. It grows while a consumer is stopped or behind, and the log keeps what it has not applied."),
		metric.WithUnit("s")))
	deadLettersGauge = must(meter.Int64ObservableGauge("bearing.deadletter.retained",
		metric.WithDescription("Dead-letter records still retained in core/dead_letter: events a worker gave up on and nobody has released. Any rise needs a person."),
		metric.WithUnit("{event}")))
)

var schedulerSyncs = must(meter.Int64Counter("bearing.scheduler.syncs",
	metric.WithDescription("Scheduled syncs by source and result: requested, skipped (the previous one is still waiting to be applied) or failed (the log refused)."),
	metric.WithUnit("{sync}")))

var retentionTrimmed = must(meter.Int64Counter("bearing.retention.trimmed",
	metric.WithDescription("Events removed from the log by the retention job."),
	metric.WithUnit("{event}")))

// eventTypeLabel names an event type for a metric label: the types the
// server knows, and "other" for any other, which a sender can choose.
func eventTypeLabel(typ string) string {
	switch typ {
	case typeSyncRequested:
		return "sync_requested"
	case typeWebhookReceived:
		return "webhook_received"
	case typeObservationsEmitted:
		return "observations_emitted"
	case typeEventDeadLettered:
		return "event_dead_lettered"
	}
	return "other"
}

func workerAttrs(typ, result string) metric.AddOption {
	return metric.WithAttributes(attribute.String("bearing.event.type", eventTypeLabel(typ)), attrResult.String(result))
}

// observeGauges reports the server's gauges until the returned function is
// called.
func (s *Server) observeGauges() (stop func()) {
	reg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(oldestUnappliedGauge, s.oldestUnapplied.Load())
		o.ObserveInt64(deadLettersGauge, s.deadLetters.Load())
		return nil
	}, oldestUnappliedGauge, deadLettersGauge)
	if err != nil {
		return func() {}
	}
	return func() { _ = reg.Unregister() }
}

// apiRequests counts API requests by method and result. The method is one of
// the API's fixed names, never a caller's value, and no label names a caller.
var apiRequests = must(meter.Int64Counter("bearing.api.requests",
	metric.WithDescription("API requests by method and result: ok, unauthenticated, rate_limited, invalid, not_found, timeout or error. A rise in unauthenticated is a client with the wrong token, or probing; error is a fault to read the logs for."),
	metric.WithUnit("{request}")))

func apiAttrs(method contracts.Method, result string) metric.AddOption {
	return metric.WithAttributes(attribute.String("bearing.authz.method", string(method)), attrResult.String(result))
}

// apiPeersRefused counts connections the Unix socket turned away because the
// peer's UID is not the service user's or root (C-API-1).
var apiPeersRefused = must(meter.Int64Counter("bearing.api.peers_refused",
	metric.WithDescription("Connections to the local API socket refused because the peer's UID is not the service user's or root. Never labelled with the UID."),
	metric.WithUnit("{connection}")))

// apiPreAuthRefused counts what the TCP listener turned away before any
// credential was looked at (C-API-6): a connection past the total cap or the
// per-peer cap, or a request past the per-peer rate. The label is one of
// those three fixed words, never a peer's address.
var apiPreAuthRefused = must(meter.Int64Counter("bearing.api.pre_auth_refused",
	metric.WithDescription("TCP API connections and requests refused before authentication: connections (total cap), peer_connections (one peer holds too many) or peer_rate (one peer asks too fast). A steady count is a scanner or a client in a loop; a rise in connections means the cap is shared by too many honest peers."),
	metric.WithUnit("{refusal}")))

func preAuthAttrs(result string) metric.AddOption {
	return metric.WithAttributes(attrResult.String(result))
}
