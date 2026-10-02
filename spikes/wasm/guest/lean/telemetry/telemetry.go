// Package telemetry is an API-only stand-in for bearing.example/pkg/telemetry
// inside WASM guests. It has the same Logger, Tracer, Meter and Fail helpers
// but links no OpenTelemetry SDK or exporters: tracing and metrics use the
// global no-op API, and logs go to the host's "log" capability.
//
// It exists to measure what splitting pkg/telemetry into an API part
// (adapters) and a setup part (binaries) would save in module size.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/spikes/wasm/guest/capability"
)

// ScopeName matches pkg/telemetry.
const ScopeName = "bearing.example"

// Tracer returns the tracer for a Bearing package.
func Tracer(pkg string) trace.Tracer { return otel.Tracer(ScopeName + "/" + pkg) }

// Meter returns the meter for a Bearing package.
func Meter(pkg string) metric.Meter { return otel.Meter(ScopeName + "/" + pkg) }

type hostLog struct{}

func (hostLog) Write(p []byte) (int, error) {
	if _, err := capability.Call("log", "write", p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Logger returns a slog logger whose JSON records go to the host's "log"
// capability, which attaches adapter, source and trace.
func Logger(pkg string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(hostLog{}, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With("scope", ScopeName+"/"+pkg)
}

// Fail records err on span and logs it, like pkg/telemetry.Fail.
func Fail(ctx context.Context, span trace.Span, log *slog.Logger, msg string, err error, attrs ...attribute.KeyValue) error {
	if err == nil {
		return nil
	}
	typ := ErrorType(err)
	span.RecordError(err, trace.WithAttributes(attrs...))
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(attribute.String("error.type", typ))
	args := []any{"error", err.Error(), "error.type", typ}
	for _, a := range attrs {
		args = append(args, string(a.Key), a.Value.AsInterface())
	}
	log.ErrorContext(ctx, msg, args...)
	return err
}

// ErrorType returns a low-cardinality description of err.
func ErrorType(err error) string {
	var t interface{ ErrorType() string }
	if errors.As(err, &t) {
		return t.ErrorType()
	}
	return fmt.Sprintf("%T", err)
}
