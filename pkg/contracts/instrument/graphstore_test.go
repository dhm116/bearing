package instrument_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"bearing.example/internal/memstore"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
	"bearing.example/pkg/contracts/instrument"
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

// The wrapper must not change behavior: it passes the same conformance suite
// as the store it wraps.
func TestWrappedStoreConforms(t *testing.T) {
	conformance.GraphStore(t, func(*testing.T) contracts.GraphStore {
		return instrument.GraphStore(memstore.New(), "memory")
	})
}

func TestWrappedIndexConforms(t *testing.T) {
	conformance.VectorIndex(t, func(*testing.T) contracts.VectorIndex {
		return instrument.VectorIndex(memstore.New(), "memory")
	}, nil)
}

func TestNotFoundIsNotAnError(t *testing.T) {
	s := instrument.GraphStore(memstore.New(), "memory")
	if _, err := s.GetEntity(context.Background(), "missing"); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	ended := spans.Ended()
	last := ended[len(ended)-1]
	if last.Name() != "graph.get_entity" {
		t.Fatalf("last span %q", last.Name())
	}
	if last.Status().Code == codes.Error {
		t.Fatal("a not-found lookup marked the span as an error")
	}
}

func TestGraphMetrics(t *testing.T) {
	conformance.GraphStore(t, func(*testing.T) contracts.GraphStore {
		return instrument.GraphStore(memstore.New(), "memory")
	})
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
		"bearing.graph.operation.duration",
		"bearing.graph.facts.written",
		"bearing.graph.facts.retracted",
		"bearing.graph.key.lookups",
	} {
		if !seen[name] {
			t.Errorf("metric %s was not recorded", name)
		}
	}
}
