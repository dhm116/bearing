# Contributing

Bearing is at an early, design-heavy stage. The most useful contributions
right now are feedback on the [specification](docs/spec/) and new adapters.

## Ground rules

- Changes to the data model or protocols start with the spec in `docs/spec/`
  and, for significant decisions, an ADR in `docs/adr/`.
- Every store implementation must pass `pkg/store/storetest`.
- Adapters need read-only access to their source and must verify webhook
  signatures when the source signs them.
- Log with `telemetry.Logger`, report failures with `telemetry.Fail`, and
  add new metrics to [docs/telemetry.md](docs/telemetry.md).
- Run `make check` (what CI runs) before sending a change.
- Working with an AI coding agent? Point it at [AGENTS.md](AGENTS.md), which
  collects the commands, layout and rules in one place.

## Writing an adapter

An adapter serves one gRPC service, `AdapterService` in
`proto/bearing/adapter/v1alpha1/adapter.proto`
([ADR 4](docs/adr/0004-protobuf-and-grpc-adapters.md)). In Go, implement
`adapter.Adapter` (`Describe`, `Sync`, `Handle`) and make `main`
`os.Exit(adapter.Main(name, version, a))`; [`adapters/github`](adapters/github)
and [`adapters/kubernetes`](adapters/kubernetes) are worked examples. In
another language, generate a gRPC server from that file and listen where
`BEARING_ADAPTER_LISTEN` says. An adapter on a `tcp://` address (a remote
one) must demand the bearer token in `BEARING_ADAPTER_TOKEN` (`adapter.Main`
refuses to listen without it) and sit behind TLS. Try it with
`bearing adapter describe -- <your binary>` and `bearing adapter sync`.
