// Package instrument wraps the stores Bearing runs on with OpenTelemetry
// spans, metrics and error logs, so every backend gets the same telemetry
// without instrumenting itself.
package instrument

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

const pkgName = "pkg/contracts/instrument"

var (
	tracer = telemetry.Tracer(pkgName)
	meter  = telemetry.Meter(pkgName)

	graphDuration = must(meter.Float64Histogram("bearing.graph.operation.duration",
		metric.WithDescription("Duration of graph store operations, by operation."),
		metric.WithUnit("s")))
)

const (
	attrBackend   = attribute.Key("db.system.name")
	attrOperation = attribute.Key("db.operation.name")
	attrResult    = attribute.Key("bearing.result")
	attrCount     = attribute.Key("bearing.results.count")
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Graph wraps s so every call is traced and measured.
func Graph(s graph.Store) graph.Store { return &graphStore{next: s} }

type graphStore struct{ next graph.Store }

// observe runs fn in a span and records its duration. ErrNotFound is an
// expected outcome, so it doesn't mark the span.
func (g *graphStore) observe(ctx context.Context, op string, fn func(context.Context) error) error {
	base := []attribute.KeyValue{attrOperation.String(op)}
	ctx, span := tracer.Start(ctx, "graph."+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(base...))
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	if err != nil && !errors.Is(err, graph.ErrNotFound) {
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "graph store operation failed", err, base...)
	}
	graphDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
	return err
}

// count records how many results a query returned on its span.
func count[T any](ctx context.Context, out []T) {
	trace.SpanFromContext(ctx).SetAttributes(attrCount.Int(len(out)))
}

// Ingest implements graph.Store.
func (g *graphStore) Ingest(ctx context.Context, source string, obs []*eventv1alpha1.Observation) (res graph.IngestResult, err error) {
	err = g.observe(ctx, "ingest", func(ctx context.Context) error {
		res, err = g.next.Ingest(ctx, source, obs)
		count(ctx, obs)
		return err
	})
	return res, err
}

// Entity implements graph.Store.
func (g *graphStore) Entity(ctx context.Context, ref string) (e graph.Entity, err error) {
	err = g.observe(ctx, "entity", func(ctx context.Context) error {
		e, err = g.next.Entity(ctx, ref)
		return err
	})
	return e, err
}

// Find implements graph.Store.
func (g *graphStore) Find(ctx context.Context, query string, kind model.Kind, limit int) (out []graph.Match, err error) {
	err = g.observe(ctx, "find", func(ctx context.Context) error {
		out, err = g.next.Find(ctx, query, kind, limit)
		count(ctx, out)
		return err
	})
	return out, err
}

// Facts implements graph.Store.
func (g *graphStore) Facts(ctx context.Context, f graph.FactFilter) (out []graph.Fact, err error) {
	err = g.observe(ctx, "facts", func(ctx context.Context) error {
		out, err = g.next.Facts(ctx, f)
		count(ctx, out)
		return err
	})
	return out, err
}

// Walk implements graph.Store.
func (g *graphStore) Walk(ctx context.Context, r graph.WalkRequest) (out []graph.Step, err error) {
	err = g.observe(ctx, "walk", func(ctx context.Context) error {
		out, err = g.next.Walk(ctx, r)
		count(ctx, out)
		return err
	})
	return out, err
}

// Changes implements graph.Store.
func (g *graphStore) Changes(ctx context.Context, r graph.ChangesRequest) (out []graph.Change, err error) {
	err = g.observe(ctx, "changes", func(ctx context.Context) error {
		out, err = g.next.Changes(ctx, r)
		count(ctx, out)
		return err
	})
	return out, err
}

// Close implements graph.Store.
func (g *graphStore) Close() error { return g.next.Close() }
