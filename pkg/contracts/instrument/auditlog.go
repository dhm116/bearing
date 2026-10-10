package instrument

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/telemetry"
)

var auditDuration = must(meter.Float64Histogram("bearing.audit.operation.duration",
	metric.WithDescription("Duration of audit log reads, by operation and backend."),
	metric.WithUnit("s")))

// AuditLog wraps l so every call is traced and measured. backend names the
// implementation (for example "memory" or "postgresql") in telemetry. The
// log is written inside GraphStore.Apply, which instrument.GraphStore
// measures (bearing.audit.records); this wraps only the reads.
func AuditLog(l contracts.AuditLog, backend string) contracts.AuditLog {
	return &auditLog{next: l, backend: backend}
}

type auditLog struct {
	next    contracts.AuditLog
	backend string
}

// observe runs fn in a span and records its duration. A filter outside the
// contract is the caller's mistake, so it doesn't mark the span.
func (a *auditLog) observe(ctx context.Context, op string, fn func(context.Context) error) error {
	base := []attribute.KeyValue{attrBackend.String(a.backend), attrOperation.String(op)}
	ctx, span := tracer.Start(ctx, "audit."+op, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(base...))
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	if err != nil && !errors.Is(err, contracts.ErrInvalidAuditQuery) {
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "audit log operation failed", err, base...)
	}
	auditDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
	return err
}

// Query implements contracts.AuditLog.
func (a *auditLog) Query(ctx context.Context, f contracts.AuditFilter) (out []*modelv1alpha1.AuditRecord, err error) {
	err = a.observe(ctx, "query", func(ctx context.Context) error {
		out, err = a.next.Query(ctx, f)
		count(ctx, out)
		return err
	})
	return out, err
}

// Head implements contracts.AuditLog.
func (a *auditLog) Head(ctx context.Context) (h contracts.AuditHead, err error) {
	err = a.observe(ctx, "head", func(ctx context.Context) error {
		h, err = a.next.Head(ctx)
		return err
	})
	return h, err
}
