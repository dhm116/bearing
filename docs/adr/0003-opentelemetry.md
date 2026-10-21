# 3. OpenTelemetry for logs, traces and metrics

Date: 2026-10-20 · Status: accepted

## Context

A sync crosses processes: the server, an adapter, and the source system's
API. Following one sync across them needs shared telemetry from day one.

## Decision

- Log through `log/slog` backed by the OpenTelemetry SDK
  (`telemetry.Logger`). No other logging library.
- Report failures with `telemetry.Fail`, which records the error on the span
  and logs it with the same attributes.
- Propagate trace context over the adapter gRPC calls, so one sync is one
  trace.
- Configure with the standard `OTEL_*` variables. Default: logs to stderr,
  traces and metrics off unless an OTLP endpoint is set.
- New spans and metrics are listed in `docs/telemetry.md`.

## Consequences

- Any OTLP backend works without code changes.
- Console output never goes to stdout, which belongs to command output.
