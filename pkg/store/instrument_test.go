package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	_ "bearing.example/internal/duckstore" // registers mem and duckdb
	"bearing.example/pkg/graph"
	"bearing.example/pkg/store"
)

var spans = tracetest.NewSpanRecorder()

func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
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
