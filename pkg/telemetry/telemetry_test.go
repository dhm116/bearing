package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// Runs first: an invalid setting must fail before any global is replaced.
func TestSetupRejectsUnknownExporter(t *testing.T) {
	_, err := Setup(context.Background(), Config{ServiceName: "test", Getenv: env(map[string]string{
		"OTEL_TRACES_EXPORTER": "carrier-pigeon",
	})})
	if err == nil || !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Fatalf("got %v, want an unsupported exporter error", err)
	}
}

func TestConsoleTelemetryGoesToStderrWriter(t *testing.T) {
	var stderr bytes.Buffer
	shutdown, err := Setup(context.Background(), Config{
		ServiceName: "test", ServiceVersion: "1.0", Stderr: &stderr,
		Getenv: env(map[string]string{"OTEL_TRACES_EXPORTER": "console", "BEARING_LOG_LEVEL": "info"}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, span := Tracer("test").Start(context.Background(), "unit-of-work")
	log := Logger("test")
	log.DebugContext(ctx, "too quiet to see")
	log.InfoContext(ctx, "hello from bearing")
	Fail(ctx, span, log, "it broke", fmt.Errorf("wrapped: %w", errors.New("boom")))
	span.End()
	otel.Handle(errors.New("exporter hiccup"))
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	out := stderr.String()
	for _, want := range []string{"hello from bearing", "it broke", "unit-of-work", "exporter hiccup", span.SpanContext().TraceID().String()} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr output is missing %q", want)
		}
	}
	if strings.Contains(out, "too quiet to see") {
		t.Error("debug record passed an info level filter")
	}
}

type codeErr struct{}

func (codeErr) Error() string     { return "x" }
func (codeErr) ErrorType() string { return "-32002" }

func TestErrorType(t *testing.T) {
	if got := ErrorType(fmt.Errorf("wrap: %w", codeErr{})); got != "-32002" {
		t.Errorf("typed error: got %q", got)
	}
	if got := ErrorType(context.DeadlineExceeded); got != "timeout" {
		t.Errorf("deadline: got %q", got)
	}
}
