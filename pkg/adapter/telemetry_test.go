package adapter

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	eventv1alpha1 "bearing.example/gen/go/bearing/event/v1alpha1"
)

var (
	spans   = tracetest.NewSpanRecorder()
	metrics = sdkmetric.NewManualReader()
)

// childEnv makes the test binary serve pager on stdio instead of running
// tests, so the Start tests have a real process to start. "serve" exits 0
// when stdin closes; "fail" serves the same way, then exits 1.
const childEnv = "BEARING_ADAPTER_TEST_CHILD"

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		if err := ServeStdio(context.Background(), pager{n: 1}); err != nil || mode == "fail" {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// The package's tracer and meter come from the global providers, which
	// delegate to the first providers installed. Install test ones once.
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metrics)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}

func TestTraceContextCrossesTheProtocol(t *testing.T) {
	c := connect(t, pager{n: 2})
	// Other tests share the recorder, so scope this test to its own trace.
	ctx, test := otel.Tracer("test").Start(context.Background(), "test")
	_, err := SyncAll(ctx, c, json.RawMessage(`{"Prefix":"x"}`), 10, func(*eventv1alpha1.Observation) error { return nil })
	test.End()
	if err != nil {
		t.Fatal(err)
	}
	traceID := test.SpanContext().TraceID()

	var root trace.SpanContext
	clients := map[trace.SpanID]bool{}
	var servers []sdktrace.ReadOnlySpan
	for _, s := range spans.Ended() {
		if s.SpanContext().TraceID() != traceID {
			continue
		}
		switch {
		case s.Name() == "bearing.adapter.sync":
			root = s.SpanContext()
		case s.SpanKind() == trace.SpanKindClient && s.Name() == MethodSync:
			clients[s.SpanContext().SpanID()] = true
		case s.SpanKind() == trace.SpanKindServer && s.Name() == MethodSync:
			servers = append(servers, s)
		}
	}
	if !root.IsValid() {
		t.Fatal("no bearing.adapter.sync span")
	}
	if len(servers) != 2 {
		t.Fatalf("got %d server spans, want 2", len(servers))
	}
	for _, s := range servers {
		if s.SpanContext().TraceID() != root.TraceID() {
			t.Errorf("server span is in trace %s, want %s", s.SpanContext().TraceID(), root.TraceID())
		}
		if !clients[s.Parent().SpanID()] {
			t.Errorf("server span's parent %s is not a client span", s.Parent().SpanID())
		}
	}
}

func TestSyncRecordsMetrics(t *testing.T) {
	c := connect(t, pager{n: 3})
	if _, err := SyncAll(context.Background(), c, json.RawMessage(`{}`), 10, func(*eventv1alpha1.Observation) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
		}
	}
	for _, name := range []string{
		"bearing.adapter.rpc.client.duration",
		"bearing.adapter.rpc.server.duration",
		"bearing.adapter.sync.duration",
		"bearing.adapter.sync.pages",
		"bearing.adapter.observations.emitted",
		"bearing.adapter.observations.received",
	} {
		if !seen[name] {
			t.Errorf("metric %s was not recorded", name)
		}
	}
}
