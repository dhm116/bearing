# Adapter protocol

Protocol version 0.1 (draft).

An adapter is a separate program that reads one external system and reports
what it sees as [observations](data-model.md#observations). Adapters can be
written in any language.

## Transport

- The core starts the adapter as a child process.
- Messages are [JSON-RPC 2.0](https://www.jsonrpc.org/specification), one
  JSON object per line, on the adapter's stdin (requests) and stdout
  (responses). This is the same pattern Terraform providers and MCP servers
  use.
- The adapter MUST NOT write anything else to stdout. Logs and other
  telemetry go to stderr or an OpenTelemetry endpoint (see
  [telemetry](../telemetry.md)).
- Requests are sent one at a time; the core waits for each response.
- Requests MAY carry a `_meta` object with W3C trace context
  (`traceparent`, `tracestate`) and `baggage`. Adapters SHOULD continue that
  trace so a sync shows up as one trace across processes. Adapters MUST
  ignore `_meta` keys they don't understand.

  ```json
  {"jsonrpc":"2.0","id":3,"method":"bearing.sync","params":{"config":{"org":"acme"}},
   "_meta":{"traceparent":"00-927f6d3e912a1212b0752ea99cac8388-b334571b6f44257b-01"}}
  ```
- An HTTP transport (the same JSON-RPC bodies over `POST /rpc`) is planned
  for adapters that run as long-lived services.

## Statelessness

Adapters keep no state between calls. Configuration travels with every
`bearing.sync` and `bearing.handle` request, and paging state lives in an
opaque cursor that the adapter returns and the core sends back. This lets the
core restart adapters, run several copies, or retry any call.

Secrets never appear in configuration. Configuration names the environment
variables that hold them (for example `"token_env": "GITHUB_TOKEN"`), and the
core sets those variables when it starts the adapter.

## Methods

### `bearing.describe`

No params. Returns what the adapter is and what it needs.

```json
{
  "name": "github",
  "version": "0.1.0",
  "protocol_version": "0.1",
  "emits": ["Repository", "Team", "Person"],
  "config_schema": { "type": "object", "required": ["org"], "properties": { "...": {} } },
  "access": ["repository metadata: read", "repository contents: read", "organization members: read"],
  "webhooks": true
}
```

`access` lists the permissions the adapter needs, in the source system's own
terms, so an operator can grant exactly those and nothing more. Adapters
SHOULD need read-only access only.

### `bearing.sync`

Returns one page of observations.

Params:

```json
{ "config": { "org": "acme" }, "cursor": "" }
```

Result:

```json
{ "observations": [ { "...": "..." } ], "next_cursor": "{\"phase\":\"teams\",\"page\":1}", "done": false }
```

- The first call has an empty cursor. The core calls again with
  `next_cursor` until `done` is true.
- If `done` is false, `next_cursor` MUST be non-empty and different from the
  cursor that was sent.
- A page SHOULD stay well under 32 MiB.
- The core runs a full sync on a schedule even when webhooks are configured,
  to catch missed deliveries and to backfill history.

### `bearing.handle` (optional)

Turns one webhook delivery into observations. The core receives the HTTP
request and passes it on unchanged.

Params:

```json
{ "config": { "org": "acme" }, "headers": { "X-GitHub-Event": ["membership"] }, "body": "<base64>" }
```

Result:

```json
{ "observations": [ { "...": "..." } ] }
```

- The adapter MUST verify the delivery's signature when the source signs
  webhooks. It knows the source's signing scheme; the core does not.
  (Under [ADR 9](../adr/0009-wasm-adapters.md) A6 and A15 the host
  verifies every delivery before it is logged; ingest stays off until that
  verifier ships with the first ingest transport, and until then this
  check is the only one. This spec changes in
  [M4](https://github.com/dhm116/bearing/milestone/5).)
- Events the adapter doesn't understand return an empty list, not an error.
- Adapters without webhook support return error `-32001`.

## Errors

Standard JSON-RPC codes plus:

| Code | Meaning |
| --- | --- |
| `-32602` | Invalid params, including bad config or a bad cursor |
| `-32001` | Method not supported by this adapter |
| `-32002` | The source system failed or refused the request |

Error messages SHOULD say what to fix ("check the org name and token
access"), not only what failed.

## Declarative adapters (planned)

Many sources are plain paginated REST APIs. For those, an adapter can be a
YAML file interpreted by a generic adapter binary instead of code:

```yaml
adapter: pagerduty
auth: { type: token, env: PD_API_TOKEN, access: read-only }
sync:
  - request: GET https://api.pagerduty.com/services
    paginate: offset
    each: $.services[*]
    emit:
      kind: Component
      key: "pagerduty:service/{{ .id }}"
      attributes: { name: "{{ .name }}", type: service }
      relations:
        - { type: owned_by, to: "pagerduty:team/{{ (index .teams 0).id }}" }
webhooks:
  - event: incident.triggered
    emit: { kind: Incident, key: "pagerduty:incident/{{ .data.id }}" }
```

The interpreter speaks the same protocol, so the core can't tell the
difference.

## Checking an adapter

```sh
bearing adapter describe -- ./my-adapter
bearing adapter sync --config cfg.json -- ./my-adapter | bearing validate
```
