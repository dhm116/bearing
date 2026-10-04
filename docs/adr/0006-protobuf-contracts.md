# 6. Protobuf defines every contract and data type

Date: 2026-09-29 · Status: proposed

## Context

Today the observation format is a hand-written JSON Schema
(`schema/observation.v1.schema.json`) mirrored by hand-written Go types in
`pkg/model`, and a test keeps the two in sync. The adapter protocol is
JSON-RPC described in prose. Every new language, component or field means
more hand-kept copies, and nothing checks for breaking changes.

The `validate` command and the `testdata/*.ndjson` files exist only to check
that JSON against the schema. They help a proof of concept and nothing after
it.

| Option | Pros | Cons |
| --- | --- | --- |
| JSON Schema (today) | Human-readable, no build step | Types written by hand in each language; no service definitions; weak breaking-change checks |
| Protobuf + buf + protovalidate | One source for types and services; generated clients in many languages; `buf breaking` in CI; compact binary for the WASM boundary (ADR 9) | A code generation step; binary payloads need tools to read |
| OpenAPI / JSON-RPC schemas | Good for HTTP APIs | Weak for streaming, events and in-process calls |

## Decision

- **Protobuf is the source of truth** for the data model, events, adapter
  interface, configuration resources and audit records. Files live in
  `proto/bearing/<area>/v1alpha1/` (for example
  `proto/bearing/model/v1alpha1/observation.proto`) until the MVP closes,
  then `v1`.
- **Tooling:** `buf` for linting, generation and `buf breaking` against
  `main` in CI. Generated Go code is committed under `gen/go/` so
  `go build` needs no extra tools.
- **Constraints** such as key patterns, required fields and confidence
  ranges are written as `buf.validate` annotations and checked with
  protovalidate at the edges: event ingest, config apply and adapter
  output.
- **Envelope:** events use the CloudEvents Protobuf format
  (`io.cloudevents.v1.CloudEvent`), with a Bearing message as the payload.
- **Services** (for example the adapter interface, ADR 9, and the core API)
  are Protobuf services served with Connect, which speaks gRPC, gRPC-Web and
  HTTP/JSON from one handler.
- **Humans still get JSON.** Config files, logs and debug output use the
  canonical ProtoJSON mapping, so nothing a person reads or writes is
  binary.
- **Versioning:** packages carry `v1alpha1` until the MVP closes and `v1`
  after (issue #11). From `v1`, adding fields is compatible; removing or
  renumbering one needs `v2`, which `buf breaking` enforces. `buf breaking`
  turns on when the MVP closes.

## Shape

One set of `.proto` files produces every type and client. Validation runs
wherever data enters Bearing.

![The Protobuf schemas feed buf, which generates Go code and SDKs and blocks breaking changes; protovalidate checks messages at every way in.](diagrams/adr6-protobuf.svg)

People still see JSON: the same messages render as ProtoJSON in config
files, the CLI and logs.

## Consequences

- Removed once this lands: `schema/observation.v1.schema.json`, the
  `bearing validate` command, `testdata/observations.ndjson` and the
  hand-written types in `pkg/model` (replaced by generated types plus small
  helpers such as key parsing).
- Adapter authors in other languages generate a client from the `.proto`
  files instead of reading prose.
- New dependencies: `google.golang.org/protobuf`, `connectrpc.com/connect`,
  `buf.build/go/protovalidate` (all Apache-2.0). Contributors who change
  `.proto` files need `buf`; everyone else does not.
- `docs/spec/` keeps the explanations. The `.proto` files carry the exact
  shapes, and the spec links to them instead of repeating field lists.
- Supersedes in part [ADR 3](0003-adapter-protocol.md) (JSON-RPC message
  format).
