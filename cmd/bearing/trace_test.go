package main

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"bearing.example/pkg/telemetry"
)

// `bearing server` and `bearing mcp` run for the life of the process, so they
// get no invocation span; a one-shot command does.
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
	_, span = startInvocation(t.Context(), []string{"mcp", "--server", "x"})
	span.End()
	if got := len(rec.Ended()); got != 0 {
		t.Fatalf("got %d spans for bearing mcp, want none", got)
	}
	_, span = startInvocation(t.Context(), []string{"get", "x"})
	span.End()
	if got := rec.Ended(); len(got) != 1 || got[0].Name() != "bearing get" {
		t.Fatalf("got %v, want one span named \"bearing get\"", got)
	}
}

func TestServiceNameIsBearingServerOnlyForTheServer(t *testing.T) {
	for args, want := range map[string]string{"server": "bearing-server", "get": "bearing-cli", "mcp": "bearing-cli", "": "bearing-cli"} {
		var a []string
		if args != "" {
			a = []string{args}
		}
		if got := serviceName(a); got != want {
			t.Errorf("serviceName(%v) = %q, want %q", a, got, want)
		}
	}
}

// A tool call is a trace of its own even when the process context has a span.
func TestMCPToolCallStartsANewRoot(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	ctx, parent := telemetry.Tracer("test").Start(t.Context(), "test.process-long-span")
	defer parent.End()
	s := &mcpServer{}
	s.callTool(ctx, mcpTool{Name: "noop", Call: func(context.Context, mcpCall, json.RawMessage) (any, string, error) {
		return map[string]any{}, "", nil
	}}, nil)
	for _, sp := range rec.Ended() {
		if sp.Name() == "mcp.tool noop" {
			if sp.Parent().IsValid() {
				t.Fatalf("mcp.tool has parent %v, want a new root", sp.Parent())
			}
			return
		}
	}
	t.Fatal("no mcp.tool span")
}
