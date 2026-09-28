# 4. OpenTelemetry for all logs, traces and metrics

Date: 2026-09-28 · Status: accepted

## Context

Bearing is a set of processes (the core, the CLI, one process per adapter)
talking over a protocol. Debugging a slow or failing sync means following
one request across processes and out to the source system's API. Adding
observability later would mean retrofitting every code path.

## Decision

- Use OpenTelemetry for logs, traces and metrics from the first release.
- All logging goes through `log/slog` backed by the OpenTelemetry log SDK
  (`otelslog` bridge). There is no separate logging library.
- Failures are reported with `telemetry.Fail`, which records the error on
  the span and logs it with the same attributes.
- Trace context is carried in the adapter protocol's `_meta` field, so one
  sync is one trace across processes.
- Configuration uses the standard `OTEL_*` environment variables. Default:
  logs to stderr, traces and metrics off unless an OTLP endpoint is set.
- Contract implementations get telemetry from wrappers in
  `pkg/contracts/instrument`, so every backend is measured the same way.

## Consequences

- The core now depends on the OpenTelemetry Go SDK and exporters, the first
  third-party dependencies.
- Operators can send Bearing's telemetry to any OTLP backend with no code
  changes.
- Console exporters must never write to stdout; stdout belongs to the
  adapter protocol and command output. `pkg/telemetry` enforces this.
