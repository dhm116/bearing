package auth

import (
	"go.opentelemetry.io/otel/metric"

	"bearing.example/pkg/telemetry"
)

const pkgName = "pkg/auth"

var meter = telemetry.Meter(pkgName)

var (
	tokenResults = must(meter.Int64Counter("bearing.auth.tokens",
		metric.WithDescription("Bearer tokens checked, by result: ok, unavailable (the identity provider's keys could not be had) or the reason the token was refused. A burst of one reason is a misconfigured client or an attack; never labelled with the token or the caller."),
		metric.WithUnit("{token}")))
	keyRefreshes = must(meter.Int64Counter("bearing.auth.key_refreshes",
		metric.WithDescription("Fetches of the identity provider's discovery document and keys, by result (ok, error). Errors that last mean new tokens cannot be checked once the cached keys age out."),
		metric.WithUnit("{fetch}")))
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
