// Package instrument wraps contracts implementations with OpenTelemetry
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

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/model"
	"bearing.example/pkg/telemetry"
)

const pkgName = "pkg/contracts/instrument"

var (
	tracer = telemetry.Tracer(pkgName)
	meter  = telemetry.Meter(pkgName)

	opDuration = must(meter.Float64Histogram("bearing.graph.operation.duration",
		metric.WithDescription("Duration of graph store operations, by operation and backend."),
		metric.WithUnit("s")))
	factsWritten = must(meter.Int64Counter("bearing.graph.facts.written",
		metric.WithDescription("Facts created or updated, by relation and whether they are asserted."),
		metric.WithUnit("{fact}")))
	factsRetracted = must(meter.Int64Counter("bearing.graph.facts.retracted",
		metric.WithDescription("Facts removed because a source reported them gone."),
		metric.WithUnit("{fact}")))
	keyLookups = must(meter.Int64Counter("bearing.graph.key.lookups",
		metric.WithDescription("Source keys resolved to entities, by result (hit, miss). A high miss rate means identity resolution is behind."),
		metric.WithUnit("{lookup}")))
)

const (
	attrBackend   = attribute.Key("db.system.name")
	attrOperation = attribute.Key("db.operation.name")
	attrRelation  = attribute.Key("bearing.fact.relation")
	attrAsserted  = attribute.Key("bearing.fact.asserted")
	attrEntity    = attribute.Key("bearing.entity.id")
	attrResult    = attribute.Key("bearing.result")
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// GraphStore wraps s so every call is traced and measured. backend names the
// implementation (for example "postgresql" or "memory") in telemetry.
func GraphStore(s contracts.GraphStore, backend string) contracts.GraphStore {
	return &graphStore{next: s, backend: backend}
}

type graphStore struct {
	next    contracts.GraphStore
	backend string
}

// observe runs fn in a span and records its duration. ErrNotFound is an
// expected outcome, not a failure, so it doesn't mark the span as an error.
func (g *graphStore) observe(ctx context.Context, op string, attrs []attribute.KeyValue, fn func(context.Context) error) error {
	base := []attribute.KeyValue{attrBackend.String(g.backend), attrOperation.String(op)}
	ctx, span := tracer.Start(ctx, "graph."+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(base, attrs...)...))
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	if err != nil && !errors.Is(err, contracts.ErrNotFound) {
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "graph store operation failed", err, append(base, attrs...)...)
	}
	opDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
	return err
}

func (g *graphStore) UpsertEntity(ctx context.Context, e contracts.Entity) error {
	return g.observe(ctx, "upsert_entity", []attribute.KeyValue{attrEntity.String(string(e.ID))}, func(ctx context.Context) error {
		return g.next.UpsertEntity(ctx, e)
	})
}

func (g *graphStore) GetEntity(ctx context.Context, id contracts.EntityID) (e contracts.Entity, err error) {
	err = g.observe(ctx, "get_entity", []attribute.KeyValue{attrEntity.String(string(id))}, func(ctx context.Context) error {
		e, err = g.next.GetEntity(ctx, id)
		return err
	})
	return e, err
}

func (g *graphStore) ResolveKey(ctx context.Context, key model.Key) (e contracts.Entity, err error) {
	err = g.observe(ctx, "resolve_key", nil, func(ctx context.Context) error {
		e, err = g.next.ResolveKey(ctx, key)
		result := "hit"
		if errors.Is(err, contracts.ErrNotFound) {
			result = "miss"
		}
		system, _, _, _ := key.Parse()
		keyLookups.Add(ctx, 1, metric.WithAttributes(attrResult.String(result), attribute.String("bearing.key.system", system)))
		return err
	})
	return e, err
}

func (g *graphStore) UpsertFact(ctx context.Context, f contracts.Fact) error {
	attrs := []attribute.KeyValue{attrRelation.String(string(f.Relation)), attrAsserted.Bool(f.Asserted)}
	return g.observe(ctx, "upsert_fact", attrs, func(ctx context.Context) error {
		if err := g.next.UpsertFact(ctx, f); err != nil {
			return err
		}
		factsWritten.Add(ctx, 1, metric.WithAttributes(attrs...))
		return nil
	})
}

func (g *graphStore) RetractFact(ctx context.Context, subject contracts.EntityID, rel model.RelationType, object contracts.EntityID) error {
	attrs := []attribute.KeyValue{attrRelation.String(string(rel))}
	return g.observe(ctx, "retract_fact", attrs, func(ctx context.Context) error {
		if err := g.next.RetractFact(ctx, subject, rel, object); err != nil {
			return err
		}
		factsRetracted.Add(ctx, 1, metric.WithAttributes(attrs...))
		return nil
	})
}

func (g *graphStore) Facts(ctx context.Context, q contracts.FactQuery) (facts []contracts.Fact, err error) {
	err = g.observe(ctx, "query_facts", nil, func(ctx context.Context) error {
		facts, err = g.next.Facts(ctx, q)
		trace.SpanFromContext(ctx).SetAttributes(attribute.Int("bearing.facts.count", len(facts)))
		return err
	})
	return facts, err
}

func (g *graphStore) History(ctx context.Context, subject contracts.EntityID) (h []contracts.FactVersion, err error) {
	err = g.observe(ctx, "history", []attribute.KeyValue{attrEntity.String(string(subject))}, func(ctx context.Context) error {
		h, err = g.next.History(ctx, subject)
		return err
	})
	return h, err
}
