package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Fail reports err on span and in the logs, and returns err unchanged so it
// can be used in a return statement:
//
//	return telemetry.Fail(ctx, span, log, "sync page failed", err)
//
// The span gets the error recorded, an error status and an error.type
// attribute; the log record is written at error level with the same
// attributes and is correlated to the span through ctx.
func Fail(ctx context.Context, span trace.Span, log *slog.Logger, msg string, err error, attrs ...attribute.KeyValue) error {
	if err == nil {
		return nil
	}
	typ := ErrorType(err)
	span.RecordError(err, trace.WithAttributes(attrs...))
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(semconv.ErrorTypeKey.String(typ))
	args := make([]any, 0, 2+2*len(attrs))
	args = append(args, "error", err.Error(), string(semconv.ErrorTypeKey), typ)
	for _, a := range attrs {
		args = append(args, string(a.Key), a.Value.AsInterface())
	}
	log.ErrorContext(ctx, msg, args...)
	return err
}

// Typed lets an error choose its own error.type value, for example a
// JSON-RPC error code.
type Typed interface{ ErrorType() string }

// ErrorType returns a low-cardinality description of err for the error.type
// attribute: the error's own type name unless it implements Typed.
func ErrorType(err error) string {
	var t Typed
	if errors.As(err, &t) {
		return t.ErrorType()
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return fmt.Sprintf("%T", err)
}
