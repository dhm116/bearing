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

var (
	logDuration = must(meter.Float64Histogram("bearing.eventlog.operation.duration",
		metric.WithDescription("Duration of event log operations, by operation and backend."),
		metric.WithUnit("s")))
	logAppended = must(meter.Int64Counter("bearing.eventlog.events",
		metric.WithDescription("Events offered to Append, by result (appended, duplicate). Many duplicates mean senders redeliver."),
		metric.WithUnit("{event}")))
	logTrimmed = must(meter.Int64Counter("bearing.eventlog.trimmed",
		metric.WithDescription("Events removed by Trim at the end of the retention window."),
		metric.WithUnit("{event}")))
)

const (
	attrPartition = attribute.Key("bearing.eventlog.partition")
	attrGroup     = attribute.Key("bearing.eventlog.group")
)

// EventLog wraps l so every call is traced and measured. backend names the
// implementation (for example "memory" or "postgresql") in telemetry. A
// failed Append is the case to alert on: while it fails, ingest cannot
// acknowledge webhooks.
func EventLog(l contracts.EventLog, backend string) contracts.EventLog {
	return &eventLog{next: l, backend: backend}
}

type eventLog struct {
	next    contracts.EventLog
	backend string
}

// observe runs fn in a span and records its duration. ErrNotFound is an
// expected outcome (a commit in a partition that doesn't exist yet), so it
// doesn't mark the span.
func (e *eventLog) observe(ctx context.Context, op string, attrs []attribute.KeyValue, fn func(context.Context) error) error {
	base := []attribute.KeyValue{attrBackend.String(e.backend), attrOperation.String(op)}
	ctx, span := tracer.Start(ctx, "eventlog."+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(base, attrs...)...))
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	if err != nil && !errors.Is(err, contracts.ErrNotFound) {
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "event log operation failed", err, append(base, attrs...)...)
	}
	logDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
	return err
}

// Append implements contracts.EventLog.
func (e *eventLog) Append(ctx context.Context, events []contracts.Event) (out []contracts.Appended, err error) {
	err = e.observe(ctx, "append", []attribute.KeyValue{attribute.Int("bearing.eventlog.events", len(events))}, func(ctx context.Context) error {
		out, err = e.next.Append(ctx, events)
		if err != nil {
			return err
		}
		dups := 0
		for _, a := range out {
			if a.Duplicate {
				dups++
			}
		}
		attrs := func(result string) metric.MeasurementOption {
			return metric.WithAttributes(attrBackend.String(e.backend), attrResult.String(result))
		}
		logAppended.Add(ctx, int64(len(out)-dups), attrs("appended"))
		logAppended.Add(ctx, int64(dups), attrs("duplicate"))
		return nil
	})
	return out, err
}

// Read implements contracts.EventLog.
func (e *eventLog) Read(ctx context.Context, partition contracts.Partition, after contracts.Offset, limit int) (out []contracts.Entry, err error) {
	err = e.observe(ctx, "read", []attribute.KeyValue{attrPartition.String(string(partition))}, func(ctx context.Context) error {
		out, err = e.next.Read(ctx, partition, after, limit)
		trace.SpanFromContext(ctx).SetAttributes(attrCount.Int(len(out)))
		return err
	})
	return out, err
}

// Commit implements contracts.EventLog.
func (e *eventLog) Commit(ctx context.Context, group string, partition contracts.Partition, offset contracts.Offset) error {
	return e.observe(ctx, "commit", []attribute.KeyValue{attrGroup.String(group), attrPartition.String(string(partition))}, func(ctx context.Context) error {
		return e.next.Commit(ctx, group, partition, offset)
	})
}

// Committed implements contracts.EventLog.
func (e *eventLog) Committed(ctx context.Context, group string, partition contracts.Partition) (o contracts.Offset, err error) {
	err = e.observe(ctx, "committed", []attribute.KeyValue{attrGroup.String(group), attrPartition.String(string(partition))}, func(ctx context.Context) error {
		o, err = e.next.Committed(ctx, group, partition)
		return err
	})
	return o, err
}

// Partitions implements contracts.EventLog.
func (e *eventLog) Partitions(ctx context.Context) (out []contracts.PartitionInfo, err error) {
	err = e.observe(ctx, "partitions", nil, func(ctx context.Context) error {
		out, err = e.next.Partitions(ctx)
		trace.SpanFromContext(ctx).SetAttributes(attrCount.Int(len(out)))
		return err
	})
	return out, err
}

// Trim implements contracts.EventLog.
func (e *eventLog) Trim(ctx context.Context, before time.Time, groups []string) (n int, err error) {
	err = e.observe(ctx, "trim", []attribute.KeyValue{attribute.Int("bearing.eventlog.groups", len(groups))}, func(ctx context.Context) error {
		n, err = e.next.Trim(ctx, before, groups)
		if err == nil {
			logTrimmed.Add(ctx, int64(n), metric.WithAttributes(attrBackend.String(e.backend)))
		}
		return err
	})
	return n, err
}

// Release implements contracts.EventLog.
func (e *eventLog) Release(ctx context.Context, ids []string) error {
	return e.observe(ctx, "release", []attribute.KeyValue{attrCount.Int(len(ids))}, func(ctx context.Context) error {
		return e.next.Release(ctx, ids)
	})
}
