package instrument

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/telemetry"
)

var auditCut = must(meter.Int64Counter("bearing.audit.cut.records",
	metric.WithDescription("Audit records removed by retention cuts."),
	metric.WithUnit("{record}")))

// AuditRetention wraps r so every cut is traced and measured. backend names
// the implementation in telemetry.
func AuditRetention(r contracts.AuditRetention, backend string) contracts.AuditRetention {
	return &auditRetention{next: r, backend: backend}
}

type auditRetention struct {
	next    contracts.AuditRetention
	backend string
}

// Cut implements contracts.AuditRetention. A request outside the contract is
// the caller's mistake; a cut that does not match the log is marked, since it
// means the log is not the one the checkpoint describes.
func (a *auditRetention) Cut(ctx context.Context, c contracts.AuditCut) (removed uint64, err error) {
	base := []attribute.KeyValue{attrBackend.String(a.backend), attrOperation.String("cut")}
	ctx, span := tracer.Start(ctx, "audit.cut", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(base...))
	defer span.End()
	start := time.Now()
	removed, err = a.next.Cut(ctx, c)
	if err != nil && !errors.Is(err, contracts.ErrInvalidAuditCut) {
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "audit log cut failed", err, base...)
	}
	if err == nil {
		span.SetAttributes(attrCount.Int(int(min(removed, 1<<31))))
		auditCut.Add(ctx, int64(min(removed, 1<<62)), metric.WithAttributes(attrBackend.String(a.backend))) //nolint:gosec // G115: capped
	}
	auditDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
	return removed, err
}
