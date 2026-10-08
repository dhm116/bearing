package instrument_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/types/known/anypb"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
	"bearing.example/internal/memstore"
	"bearing.example/internal/testkit"
	"bearing.example/pkg/contracts"
	"bearing.example/pkg/contracts/conformance"
	"bearing.example/pkg/contracts/instrument"
	"bearing.example/pkg/model"
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

func newStore(*testing.T) (contracts.GraphStore, conformance.Clock, conformance.IDs) {
	clk := testkit.NewClock(time.Time{})
	s := memstore.New()
	ids := testkit.NewUUIDv7s(clk.Now)
	s.Now, s.IDs = clk.Now, ids
	return instrument.GraphStore(s, "memory", "github"), clk, ids
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
	s, _, _ := newStore(t)
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
		"bearing.graph.state_entry.bytes",
	} {
		if !seen[name] {
			t.Errorf("metric %s was not recorded", name)
		}
	}
}

// Keys come from outside, so only configured namespaces become metric
// labels; spans keep the real one.
func TestKeyLookupsLabelUnconfiguredNamespacesOther(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	for _, k := range []model.Key{"github:team/acme/payments", "attacker-1234:team/x"} {
		if _, err := s.ResolveKey(ctx, k, time.Time{}, time.Time{}); !errors.Is(err, contracts.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	}
	if got := spans.Ended()[len(spans.Ended())-1].Attributes(); !slices.Contains(got, attribute.String("bearing.key.namespace", "attacker-1234")) {
		t.Fatalf("got span attributes %v, want the real namespace", got)
	}
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "bearing.graph.key.lookups" {
				for _, dp := range sum.DataPoints {
					v, _ := dp.Attributes.Value("bearing.key.namespace")
					labels[v.AsString()] = true
				}
			}
		}
	}
	if len(labels) != 2 || !labels["github"] || !labels["other"] {
		t.Fatalf("got namespace labels %v, want github and other", labels)
	}
}

// A state entry's size is recorded under the resolver's key prefix, and any
// other prefix is "other", so keys can't grow the label set.
func TestStateEntryBytesLabelsByPrefix(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	val := func(n int) *anypb.Any { return &anypb.Any{TypeUrl: "t", Value: make([]byte, n)} }
	cs := &modelv1alpha1.ChangeSet{EventId: "state-sizes", State: []*modelv1alpha1.StateEntry{
		{Key: "sup/github/a/owned_by/b", Value: val(3000)},
		{Key: "sup/github/a/owned_by/c", Value: val(5000)},
		{Key: "wm/github/a/out/owned_by", Value: val(700)},
		{Key: "attacker-1234/x", Value: val(9)},
	}}
	if _, err := s.Apply(ctx, cs); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if h, ok := m.Data.(metricdata.Histogram[int64]); ok && m.Name == "bearing.graph.state_entry.bytes" {
				for _, dp := range h.DataPoints {
					v, _ := dp.Attributes.Value("bearing.state.prefix")
					got[v.AsString()] += dp.Sum
				}
			}
		}
	}
	for prefix, min := range map[string]int64{"sup": 8000, "wm": 700} {
		if got[prefix] < min {
			t.Errorf("prefix %s: got %d bytes recorded, want at least %d", prefix, got[prefix], min)
		}
	}
	if _, ok := got["attacker-1234"]; ok {
		t.Errorf("got labels %v, want an unknown prefix recorded as other", got)
	}
	if got["other"] < 9 {
		t.Errorf("got %d bytes under other, want at least 9", got["other"])
	}
}
