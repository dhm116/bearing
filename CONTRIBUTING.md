# Contributing

Bearing is at an early, design-heavy stage. The most useful contributions
right now are feedback on the [specification](docs/spec/) and new adapters.

## Ground rules

- Changes to the data model or protocols start with the spec in `docs/spec/`
  and, for significant decisions, an ADR in `docs/adr/`.
- Every store implementation must pass `pkg/store/storetest`.
- Adapters need read-only access to their source. They don't check webhook
  signatures: the server checks each delivery against the scheme the
  adapter declares before logging it, and never gives the adapter the
  webhook secret.
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
`BEARING_ADAPTER_LISTEN` says. An adapter the server starts listens on a
private `unix://` socket and needs nothing else. A remote adapter listens
on `tcp://host:port` and serves mutual TLS (TLS 1.3), with three PEM files
named by variables: `BEARING_ADAPTER_TLS_CERT` and `BEARING_ADAPTER_TLS_KEY`
(its certificate and key; the key readable by its owner only) and
`BEARING_ADAPTER_TLS_CLIENT_CA` (the CA whose client certificates may call
it; that CA must issue only Bearing core certificates, because any
certificate from it can call `Sync` and `Handle`). `adapter.Main` refuses to listen without all three; the server's
settings name the matching `tls.ca_file`, `tls.cert_file` and
`tls.key_file`. Certificates are read at start, so a new one needs a
restart. Try it with
`bearing adapter describe -- <your binary>` and `bearing adapter sync`.
