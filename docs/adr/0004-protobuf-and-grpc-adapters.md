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

- The service has one integer protocol version, `DescribeResponse.protocol_version`
  (`adapter.ProtocolVersion`, now 1), which the adapter library sends in
  every answer. Additive proto changes (a new field or method) do not bump
  it; a change an older peer would misread does. The core refuses an adapter
  on a version it does not support when it first calls `Describe`, and again
  at the start of each full sync. Absent means 1; once 1 is dropped,
  adapters that send nothing are refused (amended 2026-10-24; taken from
  hashicorp/go-plugin).

- An adapter can be written in any language with a gRPC library.
- Remote adapters use mutual TLS (TLS 1.3): the core and the adapter each
  present a certificate the other checks against a CA it was configured
  with, never the system roots. One identity scheme, both sides verified,
  and no shared secret to leak (amended 2026-10-22; this replaced a bearer
  token). The adapter's client CA must issue only Bearing core
  certificates: any certificate from it can call `Sync` and `Handle`. Local adapters, on the core's private Unix socket, need no TLS
  and no certificates.
