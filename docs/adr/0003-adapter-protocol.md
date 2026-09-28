# 3. Adapters are processes speaking JSON-RPC over stdio

Date: 2026-09-28 · Status: accepted

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
