package instrument

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/telemetry"
)

var (
	vectorDuration = must(meter.Float64Histogram("bearing.vector.operation.duration",
		metric.WithDescription("Duration of vector index operations, by operation and backend."),
		metric.WithUnit("s")))
	vectorPoints = must(meter.Int64Counter("bearing.vector.points.upserted",
		metric.WithDescription("Points written to the vector index."),
		metric.WithUnit("{point}")))
)

// VectorIndex wraps ix so every call is traced and measured. backend names
// the implementation (for example "qdrant" or "surrealdb") in telemetry.
func VectorIndex(ix contracts.VectorIndex, backend string) contracts.VectorIndex {
	return &vectorIndex{next: ix, backend: backend}
}

type vectorIndex struct {
	next    contracts.VectorIndex
	backend string
}

func (v *vectorIndex) observe(ctx context.Context, op string, attrs []attribute.KeyValue, fn func(context.Context) error) error {
	base := []attribute.KeyValue{attrBackend.String(v.backend), attrOperation.String(op)}
	ctx, span := tracer.Start(ctx, "vector."+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(base, attrs...)...))
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	if err != nil {
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "vector index operation failed", err, append(base, attrs...)...)
	}
	vectorDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
	return err
}

func (v *vectorIndex) Upsert(ctx context.Context, points []contracts.VectorPoint) error {
	return v.observe(ctx, "upsert", []attribute.KeyValue{attribute.Int("bearing.vector.points", len(points))}, func(ctx context.Context) error {
		if err := v.next.Upsert(ctx, points); err != nil {
			return err
		}
		vectorPoints.Add(ctx, int64(len(points)), metric.WithAttributes(attrBackend.String(v.backend)))
		return nil
	})
}

func (v *vectorIndex) Search(ctx context.Context, q contracts.VectorQuery) (hits []contracts.VectorHit, err error) {
	err = v.observe(ctx, "search", []attribute.KeyValue{attribute.Int("bearing.vector.limit", q.Limit)}, func(ctx context.Context) error {
		hits, err = v.next.Search(ctx, q)
		trace.SpanFromContext(ctx).SetAttributes(attribute.Int("bearing.vector.hits", len(hits)))
		return err
	})
	return hits, err
}

func (v *vectorIndex) DeleteByEntity(ctx context.Context, id contracts.EntityID) error {
	return v.observe(ctx, "delete_by_entity", []attribute.KeyValue{attrEntity.String(string(id))}, func(ctx context.Context) error {
		return v.next.DeleteByEntity(ctx, id)
	})
}
