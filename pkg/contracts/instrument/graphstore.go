// Package instrument wraps contracts implementations with OpenTelemetry
// spans, metrics and error logs, so every backend gets the same telemetry
// without instrumenting itself.
package instrument

import (
	"context"
	"errors"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
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
	applies = must(meter.Int64Counter("bearing.graph.applies",
		metric.WithDescription("ChangeSets applied, by result (applied, duplicate, stale, error). Many stale results mean writers contend for the apply clock."),
		metric.WithUnit("{apply}")))
	minted = must(meter.Int64Counter("bearing.graph.subjects.minted",
		metric.WithDescription("Subjects minted, by rule (observation, reference, split)."),
		metric.WithUnit("{subject}")))
	merged = must(meter.Int64Counter("bearing.graph.subjects.merged",
		metric.WithDescription("Subjects merged into another, by rule."),
		metric.WithUnit("{merge}")))
	keyLookups = must(meter.Int64Counter("bearing.graph.key.lookups",
		metric.WithDescription("Keys resolved to subjects, by result (hit, miss). A high miss rate means identity resolution is behind."),
		metric.WithUnit("{lookup}")))
)

const (
	attrBackend   = attribute.Key("db.system.name")
	attrOperation = attribute.Key("db.operation.name")
	attrSubject   = attribute.Key("bearing.subject.id")
	attrEvent     = attribute.Key("bearing.event.id")
	attrResult    = attribute.Key("bearing.result")
	attrRule      = attribute.Key("bearing.rule")
	attrCount     = attribute.Key("bearing.results.count")
	attrNamespace = attribute.Key("bearing.key.namespace")
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// GraphStore wraps s so every call is traced and measured. backend names the
// implementation (for example "postgresql" or "memory") in telemetry.
// Metrics label key lookups with the namespaces given, the configured ones,
// and every other namespace as "other", so keys from outside can't grow the
// label set; spans keep the real namespace.
func GraphStore(s contracts.GraphStore, backend string, namespaces ...string) contracts.GraphStore {
	g := &graphStore{next: s, backend: backend, namespaces: map[string]bool{}}
	for _, ns := range namespaces {
		g.namespaces[ns] = true
	}
	return g
}

type graphStore struct {
	next       contracts.GraphStore
	backend    string
	namespaces map[string]bool
}

// observe runs fn in a span and records its duration. ErrNotFound and
// ErrStale are expected outcomes, not failures, so they don't mark the span
// as an error.
func (g *graphStore) observe(ctx context.Context, op string, attrs []attribute.KeyValue, fn func(context.Context) error) error {
	base := []attribute.KeyValue{attrBackend.String(g.backend), attrOperation.String(op)}
	ctx, span := tracer.Start(ctx, "graph."+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(base, attrs...)...))
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	if err != nil && !errors.Is(err, contracts.ErrNotFound) && !errors.Is(err, contracts.ErrStale) {
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "graph store operation failed", err, append(base, attrs...)...)
	}
	opDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(base...))
	return err
}

// count records how many results a query returned on its span.
func count[T any](ctx context.Context, out []T) {
	trace.SpanFromContext(ctx).SetAttributes(attrCount.Int(len(out)))
}

// Apply implements contracts.GraphStore.
func (g *graphStore) Apply(ctx context.Context, cs *modelv1alpha1.ChangeSet) (res contracts.ApplyResult, err error) {
	err = g.observe(ctx, "apply", []attribute.KeyValue{attrEvent.String(cs.GetEventId())}, func(ctx context.Context) error {
		res, err = g.next.Apply(ctx, cs)
		result := "applied"
		switch {
		case errors.Is(err, contracts.ErrStale):
			result = "stale"
		case err != nil:
			result = "error"
		case res.Duplicate:
			result = "duplicate"
		}
		applies.Add(ctx, 1, metric.WithAttributes(attrBackend.String(g.backend), attrResult.String(result)))
		if result != "applied" {
			return err
		}
		for _, sub := range res.Minted {
			minted.Add(ctx, 1, metric.WithAttributes(attrRule.String(model.ShortName(sub.GetMintedBy().GetRule()))))
		}
		for _, m := range res.Merges {
			merged.Add(ctx, 1, metric.WithAttributes(attrRule.String(model.ShortName(m.GetRule()))))
		}
		return nil
	})
	return res, err
}

// Head implements contracts.GraphStore.
func (g *graphStore) Head(ctx context.Context) (t time.Time, err error) {
	err = g.observe(ctx, "head", nil, func(ctx context.Context) error {
		t, err = g.next.Head(ctx)
		return err
	})
	return t, err
}

// Subject implements contracts.GraphStore.
func (g *graphStore) Subject(ctx context.Context, id contracts.SubjectID, recordedAt time.Time) (s *modelv1alpha1.Subject, err error) {
	err = g.observe(ctx, "subject", []attribute.KeyValue{attrSubject.String(string(id))}, func(ctx context.Context) error {
		s, err = g.next.Subject(ctx, id, recordedAt)
		return err
	})
	return s, err
}

// ResolveKey implements contracts.GraphStore.
func (g *graphStore) ResolveKey(ctx context.Context, key model.Key, validAt, recordedAt time.Time) (s *modelv1alpha1.Subject, err error) {
	err = g.observe(ctx, "resolve_key", nil, func(ctx context.Context) error {
		s, err = g.next.ResolveKey(ctx, key, validAt, recordedAt)
		result := "hit"
		if errors.Is(err, contracts.ErrNotFound) {
			result = "miss"
		}
		namespace, _, _, _ := key.Parse()
		trace.SpanFromContext(ctx).SetAttributes(attrNamespace.String(namespace))
		if !g.namespaces[namespace] {
			namespace = "other"
		}
		keyLookups.Add(ctx, 1, metric.WithAttributes(attrResult.String(result), attrNamespace.String(namespace)))
		return err
	})
	return s, err
}

// Bindings implements contracts.GraphStore.
func (g *graphStore) Bindings(ctx context.Context, aliases []model.Key, subjects []contracts.SubjectID, recordedAt time.Time) (out []*modelv1alpha1.Binding, err error) {
	err = g.observe(ctx, "bindings", nil, func(ctx context.Context) error {
		out, err = g.next.Bindings(ctx, aliases, subjects, recordedAt)
		count(ctx, out)
		return err
	})
	return out, err
}

// Merges implements contracts.GraphStore.
func (g *graphStore) Merges(ctx context.Context, id contracts.SubjectID, recordedAt time.Time) (out []*modelv1alpha1.MergeRecord, err error) {
	err = g.observe(ctx, "merges", []attribute.KeyValue{attrSubject.String(string(id))}, func(ctx context.Context) error {
		out, err = g.next.Merges(ctx, id, recordedAt)
		count(ctx, out)
		return err
	})
	return out, err
}

// State implements contracts.GraphStore.
func (g *graphStore) State(ctx context.Context, keys []string, recordedAt time.Time) (out map[string]*anypb.Any, err error) {
	err = g.observe(ctx, "state", nil, func(ctx context.Context) error {
		out, err = g.next.State(ctx, keys, recordedAt)
		return err
	})
	return out, err
}

// Backup implements contracts.GraphStore.
func (g *graphStore) Backup(ctx context.Context, w io.Writer) error {
	return g.observe(ctx, "backup", nil, func(ctx context.Context) error { return g.next.Backup(ctx, w) })
}

// Restore implements contracts.GraphStore.
func (g *graphStore) Restore(ctx context.Context, r io.Reader) error {
	return g.observe(ctx, "restore", nil, func(ctx context.Context) error { return g.next.Restore(ctx, r) })
}
