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
| `github.webhook <event>` | internal | GitHub adapter | `bearing.github.webhook.event`, `bearing.result` |
| `HTTP GET` | client | GitHub adapter, per API call (otelhttp) | `http.response.status_code`, `url.full` |
| `graph.<operation>` | client | any `GraphStore` wrapped by `instrument.GraphStore`: `graph.apply`, `graph.head`, `graph.subject`, `graph.resolve_key`, `graph.bindings`, `graph.merges`, `graph.unmerges`, `graph.supports`, `graph.as_of`, `graph.changes`, `graph.changes_page`, `graph.last_change`, `graph.conflicts`, `graph.data_quality`, `graph.state`, `graph.backup`, `graph.restore` | `db.system.name`, `db.operation.name`; `bearing.event.id` on `graph.apply`, `bearing.subject.id` on subject reads, `bearing.key.namespace` on `graph.resolve_key`, `bearing.results.count` on list reads |
| `audit.<operation>` | client | any `AuditLog` wrapped by `instrument.AuditLog`: `audit.query`, `audit.head` | `db.system.name`, `db.operation.name`; `bearing.results.count` on query |
| `authz.authorize` | internal | any `Authorizer` wrapped by `instrument.Authorizer`, once per API call | `db.system.name`, `bearing.authz.method` (`unlisted` for a method with no entry), `bearing.authz.role` on an allow, `error.type` on a backend error. Never the caller |
| `auth.refresh_keys` | client | `auth.Verifier`, when a token needs a key it has not cached (at most one a minute) | `bearing.auth.keys` (keys fetched); error on a failed fetch. Never the token |
| `vector.<operation>` | client | any `VectorIndex` wrapped by `instrument.VectorIndex` (`vector.repoint` after a merge) | `db.system.name`, `db.operation.name`, `bearing.vector.hits` |
| `eventlog.<operation>` | client | any `EventLog` wrapped by `instrument.EventLog`: `eventlog.append`, `eventlog.read`, `eventlog.commit`, `eventlog.committed`, `eventlog.partitions`, `eventlog.trim`, `eventlog.release` | `db.system.name`, `db.operation.name`; `bearing.eventlog.events` on append, `bearing.eventlog.partition` on read, `bearing.eventlog.group` on commit and committed, `bearing.eventlog.groups` on trim, `bearing.results.count` on read and release |

`db.system.name` is the backend: `memory` or `postgresql`. The PostgreSQL backend adds no spans of its own; an apply it retries after a serialization failure or a dropped connection is still one `graph.apply` span, and one that gives up ends it with `error.type` `busy`.

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
| `bearing.ingest.deliveries` | counter | {delivery} | `bearing.source` (`unknown` for a route that is not configured, never the caller's text), `bearing.result` (`accepted`, `duplicate`, `unauthorized`, `too_large`, `rate_limited`, `unavailable`, `bad_request`) | Webhook deliveries at the host. A rise in `unauthorized` for one source is a wrong or leaked key; `unavailable` means the event log refused and senders will retry; `rate_limited` is a flood or a too-low limit |
| `bearing.github.webhooks` | counter | {delivery} | `bearing.github.webhook.event`, `bearing.result` | Accepted, ignored and rejected deliveries; rejections can mean a wrong secret or spoofing |
| `bearing.github.codeowners.lookups` | counter | {repository} | `bearing.result` (`found`, `missing`, `unreadable`) | Ownership coverage: repos with no CODEOWNERS have no declared owner; `unreadable` is a binary or truncated file, whose facts are left as they are |
| `bearing.github.ratelimit.remaining` | gauge | {request} | `bearing.github.ratelimit.resource` | Headroom before GitHub starts refusing requests |
| `http.client.request.duration` | histogram | s | OTel HTTP semantic conventions | Upstream API latency and error codes (from otelhttp) |
| `bearing.graph.operation.duration` | histogram | s | `db.system.name`, `db.operation.name`, `error.type` | Graph backend latency per operation |
| `bearing.graph.applies` | counter | {apply} | `db.system.name`, `bearing.result` (`applied`, `duplicate`, `stale`, `error`) | Apply throughput; many `stale` results mean writers contend for the apply clock, many `duplicate` mean redelivery |
| `bearing.graph.subjects.minted` | counter | {subject} | `bearing.rule` (`observation`, `reference`, `split`) | Identity growth; a burst of `reference` mints is often a misspelled key |
| `bearing.graph.subjects.merged` | counter | {merge} | `bearing.rule` (merge rule) | How identities converge, and by which evidence |
| `bearing.audit.operation.duration` | histogram | s | `db.system.name`, `db.operation.name`, `error.type` | Audit log read latency per operation. A filter outside the contract is not an error |
| `bearing.audit.records` | counter | {record} | `db.system.name`, `bearing.audit.action` | Audit records written by applied ChangeSets, by action: the log's growth, and a burst of one action is worth a look |
| `bearing.authz.decisions` | counter | {decision} | `db.system.name`, `bearing.authz.method`, `bearing.result` (`allowed`, `denied`, `error`) | Authorization decisions. A rise in `denied` is a caller without a role; `error` means the backend could not decide and the call was refused. Never labelled with the caller |
| `bearing.auth.tokens` | counter | {token} | `bearing.result` (`ok`, `unavailable`, or the refusal reason: `malformed`, `algorithm`, `key`, `signature`, `issuer`, `audience`, `expired`, `not_yet_valid`, `token_type`, `client`, `claims`) | Bearer tokens checked. A burst of one reason is a misconfigured client or an attack; `unavailable` means the identity provider's keys could not be had. Never labelled with the token or the caller |
| `bearing.auth.key_refreshes` | counter | {fetch} | `bearing.result` (`ok`, `error`) | Fetches of the identity provider's discovery document and keys. Errors that last mean new tokens cannot be checked once the cached keys age out |
| `bearing.vector.operation.duration` | histogram | s | `db.system.name`, `db.operation.name`, `error.type` | Vector index latency per operation |
| `bearing.vector.points.upserted` | counter | {point} | `db.system.name` | Indexing throughput |
| `bearing.eventlog.operation.duration` | histogram | s | `db.system.name`, `db.operation.name`, `error.type` | Event log latency per operation. A failing `append` is the one to alert on: ingest cannot acknowledge webhooks while it fails |
| `bearing.eventlog.events` | counter | {event} | `db.system.name`, `bearing.result` (`appended`, `duplicate`) | Events offered to `Append`; many `duplicate` results mean senders redeliver |
| `bearing.eventlog.trimmed` | counter | {event} | `db.system.name` | Events removed by `Trim` at the end of the retention window |
| `bearing.graph.key.lookups` | counter | {lookup} | `bearing.result` (`hit`, `miss`), `bearing.key.namespace` (a configured namespace, else `other`) | High miss rates mean identity resolution is falling behind |
| `bearing.graph.state_entry.bytes` | histogram | By | `bearing.state.prefix` (`bind`, `del`, `sup`, `wm`, else `other`) | Payload bytes of each state entry in an applied ChangeSet (a rejected `too_large` observation never reaches the store, so it shows only in the rejection audit). Entries are rewritten whole. `sup` and `bind` entries stay flat across repeated syncs, and `wm` entries grow by a watermark per sync of a scope, so a rising tail of anything but `wm` is the state growth of [#77](https://github.com/dhm116/bearing/issues/77) |

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
