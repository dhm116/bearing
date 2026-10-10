# Adapter protocol

Protocol version 0.2 (draft).

> **Transport retired.** [ADR 9](../adr/0009-wasm-adapters.md) A13 retires
> the JSON-RPC stdio transport described below. The adapter protocol moves
> to the Protobuf adapter service: WASM modules by default, and local
> processes serving the service over Connect/gRPC on a Unix socket for
> adapters that can't run as WASM. This document describes the current
> scaffold until it is rewritten for that service.

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

No params. Returns what the adapter is, what it needs and what it emits.

```json
{
  "name": "github",
  "version": "0.2.0",
  "protocol_version": "0.2",
  "issuer_type": "github",
  "kinds": [
    { "kind": "Team",
      "keys": [ { "key_type": "team_node", "class": "id" },
                { "key_type": "team", "class": "name", "per_subject": "one", "case": "insensitive" } ],
      "fields": [ { "predicate": "name", "match": "name" },
                  { "predicate": "member_of", "direction": "in", "match": "members", "authority": { "authoritative": true } } ],
      "links": [] }
  ],
  "config_schema": { "type": "object", "required": ["org"], "properties": { "...": {} } },
  "access": ["repository metadata: read", "repository contents: read", "organization members: read"],
  "webhooks": true,
  "webhook_signature": { "scheme": "hmac_sha256", "signature_header": "X-Hub-Signature-256",
                         "signature_prefix": "sha256=", "delivery_id_header": "X-GitHub-Delivery" }
}
```

`webhook_signature` (the declaration's `webhook`,
[data model](data-model.md#declarations)) says how the source signs its
deliveries, so the host can verify one before it is logged: the scheme, the
header that carries the signature, the text before the digest and the header
with the sender's delivery ID. It never holds a secret; the source's
configuration supplies the key by reference. An adapter whose source does not
sign leaves it out, and such a source cannot receive pushed events.

`issuer_type` is the kind of identifier issuer the adapter reads; it is the
default `namespace` of a source using the adapter.

`kinds` declares every kind the adapter emits. The core validates
observations against it and rejects anything undeclared (`not_declared`).
Per kind:

| Field | Meaning |
| --- | --- |
| `keys` | Key types: `key_type`, `class` (`id`: permanent, never reassigned; `name`: renamable, reusable), and for names `per_subject`, `redirects`; `case`; `issuer_type` when not the adapter's own (for example SAML NameIDs a directory issues). |
| `fields` | Every predicate the adapter claims with this kind as the observed entity: `predicate`, `direction` (`in` for relations sent with `from`), optional `match` (`exact`, `email`, `name` or `members`) when the field is identity evidence, and `authority`. Kinds and relations come only from the data model's registry; an adapter adds only its own attributes, which also give `type` and `cardinality` and are stored namespaced. |
| `links` | Key types of other systems that this system stores for the entity (`linked_ids`): `issuer_type`, `key_type`, `authority`. |

`authority` (`{ "authoritative": true }`; default not authoritative) is
the adapter's default claim to be the source of record for that field or
link. It settles conflicts and, for a
link to another system's permanent ID, lets the core merge identities
without review. Operators can override it per source. Adapters never
declare match weights or merge rules; those are core configuration. The
[data model](data-model.md#declarations) gives the rules and complete
GitHub and Authentik declarations.

These declarations are specified for the Protobuf adapter service that
replaces this transport ([ADR 9](../adr/0009-wasm-adapters.md)): they
become fields of its `DescribeResponse`. The JSON above writes enum values
in short form (`id`); on the wire they are ProtoJSON enum names.
Until the adapter service lands, the scaffold's stdio adapters do not send
these 0.2 declarations; their `bearing.describe` still returns `emits`.

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
- The last page (`done: true`) MAY carry `complete_sync: { "kinds": ["Repository"] }`:
  the sync visited every entity of those kinds the source can see. Rules
  for declaring it are in the
  [data model](data-model.md#sync-completeness).
- A page SHOULD stay well under 32 MiB.
- The core runs a full sync on a schedule even when webhooks are configured,
  to catch missed deliveries and to backfill history.
- A rejection removes only its scope ([Audit](data-model.md#audit)). A
  problem scoped to a claim (`not_declared` for a predicate,
  `domain_mismatch`, `type_mismatch`, `core_predicate`, `invalid_value`,
  `invalid_interval`) drops that claim, and the sync goes on with the rest of
  the observation (and with the snapshot scopes that covered the claim
  trimmed, see [Snapshot scopes](data-model.md#snapshot-scopes)). Any other
  problem, and an observation the host can't decode (an unknown field, a bad
  enum name), skips the whole observation without failing the page. Either
  way the sync continues, and the host counts and logs each rejection
  instead of aborting a sync that may hold thousands of good observations.
  Adapters MUST NOT rely on a rejection to stop a sync.
- An observation left with no claims is still accepted: it asserts only that
  its entity exists. A dropped claim that ended a fact (`absent`) leaves the
  fact live.
- A sync that skipped any observation is not complete in the sense of
  [Sync completeness](data-model.md#sync-completeness), so the core won't
  take what it didn't see as deletions.

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

- The host verifies the delivery's signature from the adapter's
  `webhook_signature` declaration before it is logged ([ADR 9](../adr/0009-wasm-adapters.md)
  A6 and A15), so `bearing.handle` receives only deliveries that passed.
  An adapter running as a local process MAY verify again as defense in
  depth. Until the server's ingest listener ships (issue #139), no delivery
  reaches the adapter through the core, and an adapter that is handed one
  directly MUST verify it itself.
- Events the adapter doesn't understand return an empty list, not an error.
- One invalid observation in a delivery's result fails the call: a delivery
  is one event, not a sweep.
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
bearing adapter sync --config cfg.json -- ./my-adapter > obs.ndjson   # writes the valid observations; exits non-zero if any were rejected
```
