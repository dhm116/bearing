// Package instrument wraps contracts implementations with OpenTelemetry
// spans, metrics and error logs, so every backend gets the same telemetry
// without instrumenting itself.
package instrument

import (
	"context"
	"errors"
	"io"
	"strings"
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
	auditRecords = must(meter.Int64Counter("bearing.audit.records",
		metric.WithDescription("Audit records written by applied ChangeSets, by action. The log only grows, so this is its growth."),
		metric.WithUnit("{record}")))
	keyLookups = must(meter.Int64Counter("bearing.graph.key.lookups",
		metric.WithDescription("Keys resolved to subjects, by result (hit, miss). A high miss rate means identity resolution is behind."),
		metric.WithUnit("{lookup}")))
	stateEntryBytes = must(meter.Int64Histogram("bearing.graph.state_entry.bytes",
		metric.WithDescription("Payload bytes of each state entry in an applied ChangeSet, by key prefix. Entries are rewritten whole, so a size that keeps growing across syncs is the state growth of issue #77."),
		metric.WithUnit("By"),
		metric.WithExplicitBucketBoundaries(1<<6, 1<<7, 1<<8, 1<<9, 1<<10, 1<<11, 1<<12, 1<<13, 1<<14, 1<<15, 1<<16, 1<<17, 1<<18, 1<<19, 1<<20, 1<<21, 1<<22, 1<<23, 1<<24)))
)

const (
	attrBackend   = attribute.Key("db.system.name")
	attrOperation = attribute.Key("db.operation.name")
	attrSubject   = attribute.Key("bearing.subject.id")
	attrEvent     = attribute.Key("bearing.event.id")
	attrResult    = attribute.Key("bearing.result")
	attrRule      = attribute.Key("bearing.rule")
	attrAction    = attribute.Key("bearing.audit.action")
	attrCount     = attribute.Key("bearing.results.count")
	attrNamespace = attribute.Key("bearing.key.namespace")
	attrPrefix    = attribute.Key("bearing.state.prefix")
)

// statePrefixes are the first key segments the resolver gives its state
// entries (pkg/resolver). Keys come from outside the store, so any other
// prefix is "other" in the metric label.
var statePrefixes = map[string]bool{"bind": true, "del": true, "sup": true, "wm": true}

// statePrefix returns the label for a state key: its first segment if the
// resolver uses it, else "other".
func statePrefix(key string) string {
	first, _, _ := strings.Cut(key, "/")
	if statePrefixes[first] {
		return first
	}
	return "other"
}

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
		for _, e := range res.Audit {
			auditRecords.Add(ctx, 1, metric.WithAttributes(attrBackend.String(g.backend), attrAction.String(model.ShortName(e.GetAction()))))
		}
		for _, e := range cs.GetState() {
			stateEntryBytes.Record(ctx, int64(len(e.GetValue().GetValue())), metric.WithAttributes(attrPrefix.String(statePrefix(e.GetKey()))))
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

// Unmerges implements contracts.GraphStore.
func (g *graphStore) Unmerges(ctx context.Context, id contracts.SubjectID, recordedAt time.Time) (out []*modelv1alpha1.UnmergeRecord, err error) {
	err = g.observe(ctx, "unmerges", []attribute.KeyValue{attrSubject.String(string(id))}, func(ctx context.Context) error {
		out, err = g.next.Unmerges(ctx, id, recordedAt)
		count(ctx, out)
		return err
	})
	return out, err
}

// Supports implements contracts.GraphStore.
func (g *graphStore) Supports(ctx context.Context, f contracts.SupportFilter, recordedAt time.Time) (out []*modelv1alpha1.SupportTimeline, err error) {
	err = g.observe(ctx, "supports", nil, func(ctx context.Context) error {
		out, err = g.next.Supports(ctx, f, recordedAt)
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

// AsOf implements contracts.GraphStore.
func (g *graphStore) AsOf(ctx context.Context, f contracts.FactFilter, validAt, recordedAt time.Time) (out []*modelv1alpha1.FactState, err error) {
	err = g.observe(ctx, "as_of", nil, func(ctx context.Context) error {
		out, err = g.next.AsOf(ctx, f, validAt, recordedAt)
		count(ctx, out)
		return err
	})
	return out, err
}

// Changes implements contracts.GraphStore.
func (g *graphStore) Changes(ctx context.Context, f contracts.FactFilter, t1, t2 time.Time, axis contracts.Axis) (out []*modelv1alpha1.FactChange, err error) {
	err = g.observe(ctx, "changes", nil, func(ctx context.Context) error {
		out, err = g.next.Changes(ctx, f, t1, t2, axis)
		count(ctx, out)
		return err
	})
	return out, err
}

// ChangesPage implements contracts.GraphStore.
func (g *graphStore) ChangesPage(ctx context.Context, r contracts.ChangesRequest) (out contracts.ChangesPage, err error) {
	err = g.observe(ctx, "changes_page", nil, func(ctx context.Context) error {
		out, err = g.next.ChangesPage(ctx, r)
		count(ctx, out.Changes)
		return err
	})
	return out, err
}

// LastChange implements contracts.GraphStore.
func (g *graphStore) LastChange(ctx context.Context, f contracts.FactFilter, t time.Time, axis contracts.Axis) (out time.Time, err error) {
	err = g.observe(ctx, "last_change", nil, func(ctx context.Context) error {
		out, err = g.next.LastChange(ctx, f, t, axis)
		return err
	})
	return out, err
}

// subjectAttr tags a read with the subject it asks about, if any.
func subjectAttr(id contracts.SubjectID) []attribute.KeyValue {
	if id == "" {
		return nil
	}
	return []attribute.KeyValue{attrSubject.String(string(id))}
}

// Conflicts implements contracts.GraphStore.
func (g *graphStore) Conflicts(ctx context.Context, subject contracts.SubjectID, predicate string, validAt, recordedAt time.Time) (out []*modelv1alpha1.Conflict, err error) {
	err = g.observe(ctx, "conflicts", subjectAttr(subject), func(ctx context.Context) error {
		out, err = g.next.Conflicts(ctx, subject, predicate, validAt, recordedAt)
		count(ctx, out)
		return err
	})
	return out, err
}

// DataQuality implements contracts.GraphStore.
func (g *graphStore) DataQuality(ctx context.Context, f contracts.IssueFilter, validAt, recordedAt time.Time) (out []*modelv1alpha1.DataQualityIssue, err error) {
	err = g.observe(ctx, "data_quality", nil, func(ctx context.Context) error {
		out, err = g.next.DataQuality(ctx, f, validAt, recordedAt)
		count(ctx, out)
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
