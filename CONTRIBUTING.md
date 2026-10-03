# Contributing

Bearing is at an early, design-heavy stage. The most useful contributions
right now are feedback on the [specification](docs/spec/) and new adapters.

## Ground rules

- Changes to the data model or protocols start with the spec in `docs/spec/`
  and, for significant decisions, an ADR in `docs/adr/`.
- Every backend for a `pkg/contracts` interface must pass its conformance
  suite.
- Adapters need read-only access to their source and must verify webhook
  signatures when the source signs them.
- Log with `telemetry.Logger`, report failures with `telemetry.Fail`, and
  add new metrics to [docs/telemetry.md](docs/telemetry.md).
- Run `make check` (what CI runs) before sending a change.
- Working with an AI coding agent? Point it at [AGENTS.md](AGENTS.md), which
  collects the commands, layout and rules in one place.

## Writing an adapter

Read the [adapter protocol](docs/spec/adapter-protocol.md). In Go, implement
`adapter.Adapter` and call `adapter.ServeStdio` from `main`;
[`adapters/github`](adapters/github) is the worked example. In other
languages, speak the JSON-RPC protocol directly.
