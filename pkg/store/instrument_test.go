package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	_ "bearing.example/internal/duckstore" // registers mem and duckdb
	"bearing.example/pkg/graph"
	"bearing.example/pkg/store"
)

var (
	spans   = tracetest.NewSpanRecorder()
	metrics = sdkmetric.NewManualReader()
)

func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metrics)))
	os.Exit(m.Run())
}

func TestGraphTracesCallsAndIgnoresNotFound(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{URL: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := st.Graph()

	if _, err := s.Entity(ctx, "missing"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if _, err := s.Find(ctx, "x", "", 0); err != nil {
		t.Fatal(err)
	}
	ended := spans.Ended()
	if len(ended) < 2 {
		t.Fatalf("got %d spans, want 2", len(ended))
	}
	for i, want := range []string{"graph.entity", "graph.find"} {
		sp := ended[len(ended)-2+i]
		if sp.Name() != want {
			t.Fatalf("got span %q, want %q", sp.Name(), want)
		}
		if sp.Status().Code == codes.Error {
			t.Fatalf("span %s marked as an error", sp.Name())
		}
	}
}

// An event log call with no span in its context starts none (the server's
// idle polls would each be a trace of their own) but is still measured; with
// a span in the context it is a child.
func TestEventLogStartsNoRootSpanButRecordsTheMetric(t *testing.T) {
	st, err := store.Open(t.Context(), store.Config{URL: "mem://"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	before := len(spans.Ended())
	if _, err := st.Events().Partitions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(spans.Ended()) - before; got != 0 {
		t.Errorf("a call with no span in ctx made %d spans, want none", got)
	}
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	var recorded bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == "bearing.eventlog.operation.duration" {
				for _, dp := range h.DataPoints {
					if v, ok := dp.Attributes.Value("db.operation.name"); ok && v.AsString() == "partitions" && dp.Count > 0 {
						recorded = true
					}
				}
			}
		}
	}
	if !recorded {
		t.Error("no duration recorded for the span-less partitions call")
	}

	ctx, parent := otel.Tracer("test").Start(t.Context(), "test.parent")
	if _, err := st.Events().Partitions(ctx); err != nil {
		t.Fatal(err)
	}
	parent.End()
	var child bool
	for _, sp := range spans.Ended()[before:] {
		child = child || (sp.Name() == "eventlog.partitions" && sp.Parent().SpanID() == parent.SpanContext().SpanID())
	}
	if !child {
		t.Error("no eventlog.partitions span under the caller's span")
	}
}

func TestSpanContextOfKeepsTheTraceparentWhenOnlyTheTracestateIsBad(t *testing.T) {
	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	big := "a=" + strings.Repeat("b", 600)
	if sc := store.SpanContextOf(tp, big); !sc.IsValid() || sc.TraceState().Len() != 0 {
		t.Errorf("got %v, want a valid span context with the oversized tracestate dropped", sc)
	}
	if sc := store.SpanContextOf("00-bad", ""); sc.IsValid() {
		t.Error("a malformed traceparent gave a valid span context")
	}
	if p, s := store.CleanTrace(tp, big); p != tp || s != "" {
		t.Errorf("CleanTrace = %q %q, want the traceparent and no tracestate", p, s)
	}
}
