package store

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/graph"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

const pkgName = "pkg/store"

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

// instrumentGraph wraps g so every call is traced and measured, whatever the
// backend.
func instrumentGraph(g graph.Graph) graph.Graph { return &graphStore{next: g} }

type graphStore struct{ next graph.Graph }

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

// Ingest implements graph.Graph.
func (g *graphStore) Ingest(ctx context.Context, r graph.IngestRequest) (res graph.IngestResult, err error) {
	err = g.observe(ctx, "ingest", func(ctx context.Context) error {
		res, err = g.next.Ingest(ctx, r)
		count(ctx, r.Observations)
		return err
	})
	return res, err
}

// Entity implements graph.Graph.
func (g *graphStore) Entity(ctx context.Context, ref string) (e graph.Entity, err error) {
	err = g.observe(ctx, "entity", func(ctx context.Context) error {
		e, err = g.next.Entity(ctx, ref)
		return err
	})
	return e, err
}

// Find implements graph.Graph.
func (g *graphStore) Find(ctx context.Context, query string, kind model.Kind, limit int) (out []graph.Match, err error) {
	err = g.observe(ctx, "find", func(ctx context.Context) error {
		out, err = g.next.Find(ctx, query, kind, limit)
		count(ctx, out)
		return err
	})
	return out, err
}

// Facts implements graph.Graph.
func (g *graphStore) Facts(ctx context.Context, f graph.FactFilter) (out []graph.Fact, err error) {
	err = g.observe(ctx, "facts", func(ctx context.Context) error {
		out, err = g.next.Facts(ctx, f)
		count(ctx, out)
		return err
	})
	return out, err
}

// Walk implements graph.Graph.
func (g *graphStore) Walk(ctx context.Context, r graph.WalkRequest) (out []graph.Step, err error) {
	err = g.observe(ctx, "walk", func(ctx context.Context) error {
		out, err = g.next.Walk(ctx, r)
		count(ctx, out)
		return err
	})
	return out, err
}

// Changes implements graph.Graph.
func (g *graphStore) Changes(ctx context.Context, r graph.ChangesRequest) (out []graph.Change, err error) {
	err = g.observe(ctx, "changes", func(ctx context.Context) error {
		out, err = g.next.Changes(ctx, r)
		count(ctx, out)
		return err
	})
	return out, err
}

// Records implements graph.Graph.
func (g *graphStore) Records(ctx context.Context, kind model.Kind) (out []graph.Record, err error) {
	err = g.observe(ctx, "records", func(ctx context.Context) error {
		out, err = g.next.Records(ctx, kind)
		count(ctx, out)
		return err
	})
	return out, err
}

// Links implements graph.Graph.
func (g *graphStore) Links(ctx context.Context, f graph.LinkFilter) (out []graph.Link, err error) {
	err = g.observe(ctx, "links", func(ctx context.Context) error {
		out, err = g.next.Links(ctx, f)
		count(ctx, out)
		return err
	})
	return out, err
}

// Link implements graph.Graph.
func (g *graphStore) Link(ctx context.Context, ps []graph.Proposal) (res graph.LinkResult, err error) {
	err = g.observe(ctx, "link", func(ctx context.Context) error {
		res, err = g.next.Link(ctx, ps)
		count(ctx, ps)
		return err
	})
	return res, err
}

// CloseLink implements graph.Graph.
func (g *graphStore) CloseLink(ctx context.Context, id string, at time.Time) error {
	return g.observe(ctx, "close_link", func(ctx context.Context) error { return g.next.CloseLink(ctx, id, at) })
}

// Decide implements graph.Graph.
func (g *graphStore) Decide(ctx context.Context, d graph.Decision) (res graph.LinkResult, err error) {
	err = g.observe(ctx, "decide", func(ctx context.Context) error {
		res, err = g.next.Decide(ctx, d)
		return err
	})
	return res, err
}
