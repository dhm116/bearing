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
| `OTEL_LOGS_EXPORTER` | `otlp`, `console`, `none` | `otlp` if an endpoint is set, else `console` for `bearing server` and `none` for other commands (they print a failed command as one `bearing: …` line) |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | as in the OTel spec | per binary |
| `BEARING_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |

Other `OTEL_EXPORTER_OTLP_*` variables (headers, per-signal endpoints,
timeouts) are honored by the OTLP exporters.

**Console output always goes to stderr.** The CLI uses stdout for command
output, so nothing telemetry-related is ever written there.

Adapters the server starts get a minimal environment with no `OTEL_*`
settings: they log to stderr, which the server captures and forwards.
`bearing adapter sync` starts an adapter with the CLI's own environment, so
it exports to the same place.

## Traces

Trace context crosses the adapter service: each gRPC call carries W3C
`traceparent` (and `tracestate`, `baggage`) headers, and the adapter's server
span continues that trace. One `bearing adapter sync` shows
up as a single trace spanning both processes and every GitHub API call.

**Each unit of work is its own trace.** `bearing server` has no span around
its run (that would be the parent of everything it ever does, one trace that
never ends); it has only a short `server.start` span around loading the
settings and assembling the server, which ends before it serves. The spans for
background work (`worker.process`, `scheduler.sync`, `match.run`,
`retention.trim`) are roots (`trace.WithNewRoot`) and never chain to one
another or to whatever span their context carried.

Causes are connected by span links, not parents:

- The appender's W3C trace context (`traceparent`, and `tracestate` if present,
  the CloudEvents distributed tracing extension) is stored with the event by
  everything that appends one: `ingest.webhook`, the API's sync request, the
  scheduler, a person's link decision, the worker when it appends a page of
  observations or a dead letter. A value that is not a valid traceparent is
  dropped, never a reason to refuse the event.
- `worker.process` links to the stored span context of the event it handles, so
  a page of observations links to the sync that produced it, and that sync's
  `worker.process` links to the `ingest.webhook`, `api.request` or
  `scheduler.sync` that asked for it.
- `match.run` links to the `worker.process` span of each sync whose last page
  woke it (at most 16; runs coalesce).

| Span | Kind | Where | Key attributes |
| --- | --- | --- | --- |
| `bearing <command>` | internal | CLI, one per invocation (none for `bearing server`) | |
| `server.start` | internal | `bearing server`, around loading the settings and assembling the server; ends before the server serves | |
| `bearing.adapter.describe` | internal | core, per `Describe` call | `bearing.adapter.name`, `bearing.adapter.protocol_version` |
| `bearing.adapter.sync` | internal | core, one per full sync | `bearing.adapter.name`, `bearing.sync.pages`, `bearing.observations.count`, `bearing.observations.rejected`, `bearing.claims.rejected` |
| `bearing.adapter.v1alpha1.AdapterService/<Method>` | client | core, per adapter call (otelconnect) | `rpc.system=grpc`, `rpc.service`, `rpc.method`, `rpc.grpc.status_code` |
| `bearing.adapter.v1alpha1.AdapterService/<Method>` | server | adapter, per call, continuing the core's trace | the same |
| `github.sync repos`, `github.sync teams` | internal | GitHub adapter, per page | `bearing.github.org`, `bearing.github.sync.phase`, `bearing.github.sync.page` |
| `kubernetes.sync <phase>` | internal | Kubernetes adapter, per list request (phases `namespaces`, `services`, `index`, `workloads`) | `bearing.kubernetes.cluster`, `bearing.kubernetes.sync.phase` |
| `authentik.sync <phase>` | internal | Authentik adapter, per list page (phases `connections`, `users`, `index`, `groups`) | `bearing.authentik.sync.phase` |
| `mcp.tool <name>` | internal | `bearing mcp`, one per tool call | `bearing.result` (`ok`, or the error code such as `not_found`). Never the arguments |
| `ingest.webhook` | server | core, one per webhook delivery, whatever its result | `bearing.source` (the configured source; absent for an unknown route), `bearing.result` |
| `worker.process` | internal | core, one per event an apply worker handles, including its retries | `bearing.source`, `bearing.event.type` (`other` for a type the server does not know), `bearing.event.id`, `bearing.eventlog.offset`, `bearing.dead_letter` when it gave up. A new root, with a link to the span that appended the event |
| `scheduler.sync` | internal | core (a new root), one per scheduled sync tick | `bearing.source` |
| `retention.trim` | internal | core (a new root), one per trim of the event log | |
| `match.run` | internal | core (a new root, linked to the `worker.process` spans of the syncs that woke it), one per matching run after a sync's last page | `bearing.source` |
| `api.request` | server | core, one per API request, whatever its answer | `bearing.request.id`, `bearing.authz.method` (including `query.runtimes`), `bearing.result`. Never the caller or the request's parameters |
| `github.webhook <event>` | internal | GitHub adapter | `bearing.github.webhook.event`, `bearing.result` |
| `HTTP GET` | client | GitHub adapter, per API call (otelhttp) | `http.response.status_code`, `url.full` |
| `HTTP GET`, `HTTP POST` | client | `bearing` command line, per request to a server (otelhttp, `pkg/apiclient`) | `http.response.status_code`, `url.full`. The question is in the URL's query; the bearer token never is |
| `graph.<operation>` | client | any graph opened through `store.Open`: `graph.ingest`, `graph.entity`, `graph.find`, `graph.facts`, `graph.walk`, `graph.changes`, `graph.records`, `graph.records_by_id`, `graph.links`, `graph.link` (open or reinforce an identity link), `graph.close_link`, `graph.decide` (a person's decision on a link) | `db.operation.name`; `bearing.results.count` on `graph.ingest` (observations given) and the list reads |
| `eventlog.<operation>` | client | any event log opened through `store.Open`: `eventlog.append`, `eventlog.read`, `eventlog.commit`, `eventlog.committed`, `eventlog.partitions`, `eventlog.trim`, `eventlog.release` | `db.system.name`, `db.operation.name`; `bearing.eventlog.events` on append, `bearing.eventlog.partition` on read, `bearing.eventlog.group` on commit and committed, `bearing.eventlog.groups` on trim, `bearing.results.count` on read and release |

`db.system.name` on the event log is `duckdb`. A failed graph call is recorded on its span with `error.type`; `graph.ErrNotFound` is an answer, not a failure.

## Metrics

| Metric | Type | Unit | Attributes | Why it matters |
| --- | --- | --- | --- | --- |
| `bearing.adapter.rpc.client.duration` | histogram | s | `rpc.method`, `error.type` | Latency the core sees per adapter call |
| `bearing.adapter.rpc.server.duration` | histogram | s | same | Time spent inside the adapter, vs. transport overhead |
| `bearing.adapter.sync.duration` | histogram | s | `bearing.adapter.name`, `error.type` | How long a full sync takes; drives schedule choice |
| `bearing.adapter.sync.pages` | counter | {page} | `bearing.adapter.name` | Sync progress and size |
| `bearing.adapter.observations.emitted` | counter | {observation} | `rpc.method`, `bearing.entity.kind` | What each adapter produces, sync vs. webhook |
| `bearing.adapter.observations.received` | counter | {observation} | `bearing.adapter.name`, `bearing.entity.kind` | What the core accepted |
| `bearing.adapter.observations.invalid` | counter | {observation} | `bearing.adapter.name` | Adapters emitting schema-invalid data; should be zero |
| `bearing.adapter.claims.rejected` | counter | {claim} | `bearing.adapter.name` | Claims removed from accepted observations; an adapter losing most of its claims is alertable |
| `bearing.ingest.deliveries` | counter | {delivery} | `bearing.source` (`unknown` for a route that is not configured, never the caller's text), `bearing.result` (`accepted`, `duplicate`, `unauthorized`, `too_large`, `rate_limited`, `busy`, `unavailable`, `bad_request`) | Webhook deliveries at the host. A rise in `unauthorized` for one source is a wrong or leaked key; `unavailable` means the event log refused (or the event could not be built) and senders will retry; `busy` is more deliveries at once than the in-flight cap (default 256), answered 503 with `Retry-After`; `rate_limited` is a flood from one peer, or a source sending faster than its limit; only signed deliveries count against a source |
| `bearing.worker.events` | counter | {event} | `bearing.event.type` (`sync_requested`, `webhook_received`, `observations_emitted`, `event_dead_lettered`, `link_decided`, else `other`), `bearing.result` (`applied`, `skipped`, `retried`, `dead_lettered`) | What the apply workers did with each event. A rise in `retried` is a source or the store failing; any `dead_lettered` needs a person; `skipped` of a type that should be handled is a handler that is missing |
| `bearing.worker.oldest_unapplied.age` | gauge | s | | Age of the oldest event the apply group has not applied. It grows while the workers are stopped or behind, and the log keeps what they have not applied (C-STORE-11) |
| `bearing.deadletter.retained` | gauge | {event} | | Dead-letter records still in `core/dead_letter`. Any rise needs a person: an event was given up on |
| `bearing.scheduler.syncs` | counter | {sync} | `bearing.source`, `bearing.result` (`requested`, `skipped`, `failed`) | Scheduled syncs. `skipped` means the last request, scheduled or manual, is still waiting to be applied, so the source is slower than its interval; `failed` means the log refused |
| `bearing.retention.trimmed` | counter | {event} | | Events removed from the log by the server's retention job |
| `bearing.match.links` | counter | {link} | `bearing.result` (`asserted`, `proposed`, `reinforced`, `closed`) | Identity links written, reinforced or closed by matching runs: asserted by a key rule, proposed (a new proposal) by a matcher, reinforced (a deposit on an existing proposal), closed when a proposal decayed below the floor |
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

A remote adapter, and the core dialling one, log a warning
(`cannot reload the TLS files; keeping the last good ones`, with `error`
naming the setting and path, and `error.type`) when a changed certificate,
key or CA file fails to load: once per change of the files, not on every
retry.

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
- A store implementation gets telemetry from `store.Open`, which wraps its
  graph and event log ([`pkg/store`](../pkg/store)); it doesn't instrument
  itself.
- Add every new metric to the table above.
