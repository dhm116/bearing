# 4. Protobuf data model; adapters speak gRPC

Date: 2026-10-20 · Status: accepted

## Context

Adapters run next to Bearing (a container in the same Compose project or
pod) or far from it (a remote cluster or account). They need one protocol
for both cases, and the data they send needs one definition that every
language can generate.

## Decision

- `proto/` is the only definition of observations, the adapter service and
  events. Generated Go code is committed under `gen/go`.
- Events use the CloudEvents envelope with a Bearing message as payload.
- Adapters are separate processes serving one gRPC service (via Connect):
  `Describe` and `Sync` (cursor paged), plus `Handle` for webhooks when the
  adapter supports them. The same service is used locally and remotely.
- Adapters are stateless and read-only against their source. They never
  resolve identities across systems; the core does.
- The stdio JSON-RPC transport is retired. WASM sandboxing is deferred (see
  the roadmap).

## Consequences

- An adapter can be written in any language with a gRPC library.
- Remote adapters need transport security and an auth token; local ones on
  a private network or Unix socket can skip TLS.
