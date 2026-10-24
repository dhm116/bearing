package main

import (
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// `bearing server` runs for the life of the process, so it gets no
// invocation span; a one-shot command does.
func TestOnlyOneShotCommandsGetAnInvocationSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	_, span := startInvocation(t.Context(), []string{"server", "--settings", "x"})
	span.End()
	if got := len(rec.Ended()); got != 0 {
		t.Fatalf("got %d spans for bearing server, want none", got)
	}
	_, span = startInvocation(t.Context(), []string{"get", "x"})
	span.End()
	if got := rec.Ended(); len(got) != 1 || got[0].Name() != "bearing get" {
		t.Fatalf("got %v, want one span named \"bearing get\"", got)
	}
}
