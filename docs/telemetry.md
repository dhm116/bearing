# Telemetry

Every Bearing binary emits OpenTelemetry logs, traces and metrics through
[`pkg/telemetry`](../pkg/telemetry). There is no other logging path: code
logs with `telemetry.Logger(pkg)` (a `log/slog` logger backed by the
OpenTelemetry log SDK), and failures are reported with `telemetry.Fail`,
which records the error on the current span, sets the span status and
`error.type`, and writes an error log correlated to the span.

## Configuration

Standard OpenTelemetry environment variables:

| Variable | Values | Default |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/HTTP endpoint, e.g. `http://localhost:4318` | unset |
| `OTEL_TRACES_EXPORTER` | `otlp`, `console`, `none` | `otlp` if an endpoint is set, else `none` |
| `OTEL_METRICS_EXPORTER` | `otlp`, `console`, `none` | `otlp` if an endpoint is set, else `none` |
| `OTEL_LOGS_EXPORTER` | `otlp`, `console`, `none` | `otlp` if an endpoint is set, else `console` |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | as in the OTel spec | per binary |
| `BEARING_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |

Other `OTEL_EXPORTER_OTLP_*` variables (headers, per-signal endpoints,
timeouts) are honored by the OTLP exporters.

**Console output always goes to stderr.** Adapters use stdout for the
adapter protocol, and the CLI uses it for command output, so nothing
telemetry-related is ever written there.

Adapters started by the core inherit its environment, so they export to the
same place.

## Traces

Trace context crosses the adapter protocol: the core puts W3C `traceparent`
(and `tracestate`, `baggage`) in each request's `_meta` field, and the
adapter's server span continues that trace. One `bearing adapter sync` shows
up as a single trace spanning both processes and every GitHub API call.

| Span | Kind | Where | Key attributes |
| --- | --- | --- | --- |
| `bearing <command>` | internal | CLI, one per invocation | |
| `bearing.adapter.sync` | internal | core, one per full sync | `bearing.adapter.name`, `bearing.sync.pages`, `bearing.observations.count`, `bearing.observations.rejected`, `bearing.claims.rejected` |
| `bearing.describe`, `bearing.sync`, `bearing.handle` | client | core, per protocol call | `rpc.system.name=jsonrpc`, `rpc.method`, `jsonrpc.request.id` |
| `bearing.describe`, `bearing.sync`, `bearing.handle` | server | adapter, per request | same, plus `rpc.response.status_code` on error |
| `github.sync repos`, `github.sync teams` | internal | GitHub adapter, per page | `bearing.github.org`, `bearing.github.sync.phase`, `bearing.github.sync.page` |
| `ingest.webhook` | server | core, one per webhook delivery, whatever its result | `bearing.source` (the configured source; absent for an unknown route), `bearing.result` |
| `worker.process` | internal | core, one per event an apply worker handles, including its retries | `bearing.source`, `bearing.event.type` (`other` for a type the server does not know), `bearing.eventlog.offset`, `bearing.dead_letter` when it gave up |
| `scheduler.sync` | internal | core, one per scheduled sync tick | `bearing.source` |
| `retention.trim` | internal | core, one per trim of the event log | |
| `api.request` | server | core, one per API request, whatever its answer | `bearing.request.id`, `bearing.authz.method`, `bearing.result`. Never the caller or the request's parameters |
| `github.webhook <event>` | internal | GitHub adapter | `bearing.github.webhook.event`, `bearing.result` |
| `HTTP GET` | client | GitHub adapter, per API call (otelhttp) | `http.response.status_code`, `url.full` |
| `HTTP GET`, `HTTP POST` | client | `bearing` command line, per request to a server (otelhttp, `pkg/apiclient`) | `http.response.status_code`, `url.full`. The question is in the URL's query; the bearer token never is |
| `graph.<operation>` | client | any `graph.Store` wrapped by `instrument.Graph`: `graph.ingest`, `graph.entity`, `graph.find`, `graph.facts`, `graph.walk`, `graph.changes` | `db.operation.name`; `bearing.results.count` on `graph.ingest` (observations given) and the list reads |
| `eventlog.<operation>` | client | any `EventLog` wrapped by `instrument.EventLog`: `eventlog.append`, `eventlog.read`, `eventlog.commit`, `eventlog.committed`, `eventlog.partitions`, `eventlog.trim`, `eventlog.release` | `db.system.name`, `db.operation.name`; `bearing.eventlog.events` on append, `bearing.eventlog.partition` on read, `bearing.eventlog.group` on commit and committed, `bearing.eventlog.groups` on trim, `bearing.results.count` on read and release |

`db.system.name` on the event log is `duckdb`. A failed graph call is recorded on its span with `error.type`; `graph.ErrNotFound` is an answer, not a failure.

## Metrics

| Metric | Type | Unit | Attributes | Why it matters |
| --- | --- | --- | --- | --- |
| `bearing.adapter.rpc.client.duration` | histogram | s | `rpc.method`, `bearing.adapter.name`, `error.type` | Latency the core sees per adapter call |
| `bearing.adapter.rpc.server.duration` | histogram | s | same | Time spent inside the adapter, vs. transport overhead |
| `bearing.adapter.sync.duration` | histogram | s | `bearing.adapter.name`, `error.type` | How long a full sync takes; drives schedule choice |
| `bearing.adapter.sync.pages` | counter | {page} | `bearing.adapter.name` | Sync progress and size |
| `bearing.adapter.observations.emitted` | counter | {observation} | `rpc.method`, `bearing.entity.kind` | What each adapter produces, sync vs. webhook |
| `bearing.adapter.observations.received` | counter | {observation} | `bearing.adapter.name`, `bearing.entity.kind` | What the core accepted |
| `bearing.adapter.observations.invalid` | counter | {observation} | `bearing.adapter.name` | Adapters emitting schema-invalid data; should be zero |
| `bearing.adapter.claims.rejected` | counter | {claim} | `bearing.adapter.name` | Claims removed from accepted observations; an adapter losing most of its claims is alertable |
| `bearing.ingest.deliveries` | counter | {delivery} | `bearing.source` (`unknown` for a route that is not configured, never the caller's text), `bearing.result` (`accepted`, `duplicate`, `unauthorized`, `too_large`, `rate_limited`, `busy`, `unavailable`, `bad_request`) | Webhook deliveries at the host. A rise in `unauthorized` for one source is a wrong or leaked key; `unavailable` means the event log refused (or the event could not be built) and senders will retry; `busy` is more deliveries at once than the in-flight cap (default 256), answered 503 with `Retry-After`; `rate_limited` is a flood from one peer, or a source sending faster than its limit; only signed deliveries count against a source |
| `bearing.worker.events` | counter | {event} | `bearing.event.type` (`sync_requested`, `webhook_received`, `observations_emitted`, `event_dead_lettered`, else `other`), `bearing.result` (`applied`, `skipped`, `retried`, `dead_lettered`) | What the apply workers did with each event. A rise in `retried` is a source or the store failing; any `dead_lettered` needs a person; `skipped` of a type that should be handled is a handler that is missing |
| `bearing.worker.oldest_unapplied.age` | gauge | s | | Age of the oldest event the apply group has not applied. It grows while the workers are stopped or behind, and the log keeps what they have not applied (C-STORE-11) |
| `bearing.deadletter.retained` | gauge | {event} | | Dead-letter records still in `core/dead_letter`. Any rise needs a person: an event was given up on |
| `bearing.scheduler.syncs` | counter | {sync} | `bearing.source`, `bearing.result` (`requested`, `skipped`, `failed`) | Scheduled syncs. `skipped` means the last request, scheduled or manual, is still waiting to be applied, so the source is slower than its interval; `failed` means the log refused |
| `bearing.retention.trimmed` | counter | {event} | | Events removed from the log by the server's retention job |
| `bearing.api.requests` | counter | {request} | `bearing.authz.method`, `bearing.result` (`ok`, `unauthenticated`, `rate_limited`, `invalid`, `not_found`, `timeout`, `error`) | API requests. A rise in `unauthenticated` is a client with the wrong token, or probing; `error` is a fault. Never labelled with the caller |
| `bearing.api.peers_refused` | counter | {connection} | | Connections to the local socket refused because the peer is neither the service user nor root (C-API-1). Never labelled with the UID |
| `bearing.api.pre_auth_refused` | counter | {refusal} | `bearing.result` (`connections`, `peer_connections`, `peer_rate`) | What the TCP API listener turned away before looking at a credential (C-API-6): a connection past the total cap or one peer's cap, or a request past one peer's rate. A steady count is a scanner or a client in a loop; `connections` rising with honest peers means the cap is too low for them. Never labelled with the peer |
| `bearing.adapter.restarts` | counter | {restart} | `bearing.source` | Adapter processes the server dropped after a start or call failed in transit, or outlasted its deadline. Each is followed by a pause that doubles from one second to one minute, so a rate of one a minute is an adapter that cannot run (C-OPS-5) |
| `bearing.github.webhooks` | counter | {delivery} | `bearing.github.webhook.event`, `bearing.result` | Accepted, ignored and rejected deliveries; rejections can mean a wrong secret or spoofing |
| `bearing.github.codeowners.lookups` | counter | {repository} | `bearing.result` (`found`, `missing`, `unreadable`) | Ownership coverage: repos with no CODEOWNERS have no declared owner; `unreadable` is a binary or truncated file, whose facts are left as they are |
| `bearing.github.ratelimit.remaining` | gauge | {request} | `bearing.github.ratelimit.resource` | Headroom before GitHub starts refusing requests |
| `http.client.request.duration` | histogram | s | OTel HTTP semantic conventions | Upstream API latency and error codes (from otelhttp) |
| `bearing.graph.operation.duration` | histogram | s | `db.operation.name`, `error.type` | Graph backend latency per operation |
| `bearing.eventlog.operation.duration` | histogram | s | `db.system.name`, `db.operation.name`, `error.type` | Event log latency per operation. A failing `append` is the one to alert on: ingest cannot acknowledge webhooks while it fails |
| `bearing.eventlog.events` | counter | {event} | `db.system.name`, `bearing.result` (`appended`, `duplicate`) | Events offered to `Append`; many `duplicate` results mean senders redeliver |
| `bearing.eventlog.trimmed` | counter | {event} | `db.system.name` | Events removed by `Trim` at the end of the retention window |

Attributes are deliberately low-cardinality: no subject IDs, keys or
repository names on metrics. Those belong on spans and logs.

## Logs

Log records carry the trace and span IDs of the context they were written
in, so a log line links to its trace. Error logs come from `telemetry.Fail`
and always include `error` and `error.type`. OpenTelemetry's own internal
errors (for example an exporter that can't reach its endpoint) are routed to
the same logger.

## Trying it locally

```sh
# Everything to stderr as JSON
OTEL_TRACES_EXPORTER=console OTEL_METRICS_EXPORTER=console \
  bin/bearing adapter sync --config github.json -- bin/bearing-adapter-github > obs.ndjson

# Or to any OTLP collector, for example Jaeger or Grafana's otel-lgtm image
docker run --rm -p 3000:3000 -p 4318:4318 grafana/otel-lgtm
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
  bin/bearing adapter sync --config github.json -- bin/bearing-adapter-github > obs.ndjson
```

## Adding telemetry to new code

- Get instruments from `telemetry.Tracer(pkg)`, `telemetry.Meter(pkg)` and
  `telemetry.Logger(pkg)`. They are safe to create at package init.
- Report failures with `telemetry.Fail`. Errors can implement
  `ErrorType() string` to control the `error.type` value.
- New backends for a `contracts` interface get telemetry by being wrapped
  (see [`pkg/contracts/instrument`](../pkg/contracts/instrument)), not by
  instrumenting themselves.
- Add every new metric to the table above.
