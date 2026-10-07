package instrument_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
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

func newStore(*testing.T) (contracts.GraphStore, conformance.Clock) {
	clk := testkit.NewClock(time.Time{})
	s := memstore.New()
	s.Now, s.NewID = clk.Now, testkit.NewUUIDs().NewID
	return instrument.GraphStore(s, "memory"), clk
}

// The wrapper must not change behavior: it passes the same conformance suite
// as the store it wraps.
func TestWrappedStoreConforms(t *testing.T) {
	conformance.GraphStore(t, newStore)
}

func TestWrappedIndexConforms(t *testing.T) {
	conformance.VectorIndex(t, func(*testing.T) contracts.VectorIndex {
		return instrument.VectorIndex(memstore.New(), "memory")
	})
}

func TestExpectedErrorsAreNotSpanErrors(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Subject(ctx, "missing", time.Time{}); !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "e"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{EventId: "f"}); !errors.Is(err, contracts.ErrStale) {
		t.Fatalf("got %v, want ErrStale", err)
	}
	ended := spans.Ended()
	for _, sp := range ended[len(ended)-3:] {
		if sp.Status().Code == codes.Error {
			t.Fatalf("span %s marked as an error", sp.Name())
		}
	}
	if name := ended[len(ended)-3].Name(); name != "graph.subject" {
		t.Fatalf("got span %q, want graph.subject", name)
	}
	if _, err := s.Apply(ctx, &modelv1alpha1.ChangeSet{}); err == nil {
		t.Fatal("applied a ChangeSet with no event ID")
	}
	if last := spans.Ended()[len(spans.Ended())-1]; last.Status().Code != codes.Error {
		t.Fatalf("got status %v, want an error", last.Status())
	}
}

func TestGraphMetrics(t *testing.T) {
	conformance.GraphStore(t, newStore)
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
		"bearing.graph.applies",
		"bearing.graph.subjects.minted",
		"bearing.graph.subjects.merged",
		"bearing.graph.key.lookups",
	} {
		if !seen[name] {
			t.Errorf("metric %s was not recorded", name)
		}
	}
}
