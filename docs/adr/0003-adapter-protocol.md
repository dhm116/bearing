# 3. Adapters are processes speaking JSON-RPC over stdio

Date: 2026-09-28 · Status: accepted · Superseded in part by [ADR 9](0009-wasm-adapters.md)

> Superseded in part by [ADR 9](0009-wasm-adapters.md) (accepted
> 2026-10-02, A13 revised 2026-10-03): the stdio JSON-RPC transport is
> retired. WASM becomes the default runtime when
> [M4](https://github.com/dhm116/bearing/milestone/5) lands, and adapters
> that can't run as WASM run as local processes serving the Protobuf
> adapter service on a Unix socket (A13). The host verifies webhook
> deliveries before they are logged; ingest stays off until that verifier
> ships with the first ingest transport, and until then the adapter's own
> verification is the check (A15). Stateless, cursor-paged adapters that
> never resolve identities across systems carry over. Proposed replacement
> for the message format: [ADR 6](0006-protobuf-contracts.md).

## Context

Bearing should not replace existing tools. It should make it easy to write
small adapters that fetch useful data from them. Backstage's experience shows
that in-process plugins tied to the host's language and release cycle become
an upgrade burden.

## Decision

- Adapters are separate executables. The core talks to them with JSON-RPC 2.0,
  one message per line, over stdin and stdout.
- Three methods: `bearing.describe`, `bearing.sync` (paged, cursor-based) and
  the optional `bearing.handle` for webhooks.
- Adapters are stateless. Config comes with every call; paging state lives in
  an opaque cursor.
- Adapters emit observations as CloudEvents in a versioned schema. They never
  resolve identities across systems.

## Consequences

- Adapters can be written in any language and upgraded independently of the
  core.
- The same pattern as Terraform providers and MCP servers, so it is familiar.
- Process start-up cost per adapter; negligible next to network calls to the
  source systems.
- An HTTP transport can be added later without changing the method contract.
