package instrument

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"bearing.example/pkg/contracts"
	"bearing.example/pkg/telemetry"
)

var authzDecisions = must(meter.Int64Counter("bearing.authz.decisions",
	metric.WithDescription("Authorization decisions by method and result (allowed, denied, error). A rise in denied is a caller without a role; error means the backend could not decide and the call was refused."),
	metric.WithUnit("{decision}")))

const (
	attrMethod = attribute.Key("bearing.authz.method")
	attrRole   = attribute.Key("bearing.authz.role")
)

// Authorizer wraps a so every decision is traced and counted. backend names
// the implementation in telemetry. The caller's groups, subject and client ID
// are never attributes (threat model C-IDP-5), and a method that is not in
// the table is recorded as "unlisted", so a caller cannot grow the label set.
func Authorizer(a contracts.Authorizer, backend string) contracts.Authorizer {
	return &authorizer{next: a, backend: backend}
}

type authorizer struct {
	next    contracts.Authorizer
	backend string
}

// Authorize implements contracts.Authorizer.
func (a *authorizer) Authorize(ctx context.Context, r contracts.Request) (contracts.AuthDecision, error) {
	method := "unlisted"
	if _, ok := contracts.RequiredRole(r.Method); ok {
		method = string(r.Method)
	}
	base := []attribute.KeyValue{attrBackend.String(a.backend), attrMethod.String(method)}
	ctx, span := tracer.Start(ctx, "authz.authorize", trace.WithAttributes(base...))
	defer span.End()
	d, err := a.next.Authorize(ctx, r)
	result := "denied"
	switch {
	case err != nil:
		result = "error"
		base = append(base, semconv.ErrorTypeKey.String(telemetry.ErrorType(err)))
		telemetry.Fail(ctx, span, telemetry.Logger(pkgName), "authorization failed", err, base...)
	case d.Allowed:
		result = "allowed"
		span.SetAttributes(attrRole.String(string(d.Role)))
	}
	authzDecisions.Add(ctx, 1, metric.WithAttributes(append(base, attrResult.String(result))...))
	if err != nil {
		// The contract says an error is a denial; make it one here, so a
		// backend that returns Allowed with an error cannot let a call through.
		return contracts.AuthDecision{}, err
	}
	return d, nil
}
