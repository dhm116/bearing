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
`os.Exit(adapter.Main(name, version, a))`; [`adapters/github`](adapters/github),
[`adapters/kubernetes`](adapters/kubernetes) and [`adapters/authentik`](adapters/authentik) are worked examples. In
another language, generate a gRPC server from that file and listen where
`BEARING_ADAPTER_LISTEN` says. An adapter the server starts listens on a
private `unix://` socket and needs nothing else. A remote adapter listens
on `tcp://host:port` and serves mutual TLS (TLS 1.3), with three PEM files
named by variables: `BEARING_ADAPTER_TLS_CERT` and `BEARING_ADAPTER_TLS_KEY`
(its certificate and key; the key readable by its owner only) and
`BEARING_ADAPTER_TLS_CLIENT_CA` (the CA whose client certificates may call
it). `adapter.Main` refuses to listen without all three; the server's
settings name the matching `tls.ca_file`, `tls.cert_file` and
`tls.key_file`. The files are read again when they change, so a rotated
certificate needs no restart.

The core refuses an adapter built for another protocol version
(`adapter.ProtocolVersion`, sent by `adapter.Main`; a non-Go adapter sets
`DescribeResponse.protocol_version`): rebuild it from the same release.

Without a peer ID, any certificate from the client CA can call `Sync` and
`Handle`, so that CA must issue only Bearing core certificates. To share a
CA (a SPIRE trust domain, say), name the SPIFFE ID each side must present:
`BEARING_ADAPTER_TLS_PEER_ID` on the adapter (the core's ID), and
`tls.peer_id` in the server's settings or `--tls-peer-id` on the CLI (the
adapter's ID, checked in place of a host name, so not with `server_name`).
With SPIRE, `spiffe-helper` keeps the three files current:

```hcl
# helper.conf, for the adapter (the core's is the same with its own paths)
agent_address = "/run/spire/agent.sock"
cert_dir = "/run/bearing-adapter/tls"
svid_file_name = "svid.pem"            # BEARING_ADAPTER_TLS_CERT
svid_key_file_name = "svid_key.pem"    # BEARING_ADAPTER_TLS_KEY
svid_bundle_file_name = "bundle.pem"   # BEARING_ADAPTER_TLS_CLIENT_CA
key_file_mode = 0600                   # Bearing refuses a key others can read
cert_file_mode = 0644
daemon_mode = true
```

Try it with
`bearing adapter describe -- <your binary>` and `bearing adapter sync`.

Secrets reach an adapter the server starts as environment variables. A
source's `spec.secrets` key is the variable the adapter sees; the value is
a reference to a variable of the server, so an SDK's own name works
unchanged and two sources can give it different values:

```json
"secrets": {"AWS_ACCESS_KEY_ID": "env:BEARING_SECRET_AWS_PROD_KEY_ID",
            "AWS_SECRET_ACCESS_KEY": "env:BEARING_SECRET_AWS_PROD_SECRET"}
```

A key is an upper-case variable name and may not be `PATH` or `GODEBUG`, or start with
`BEARING_`, `LD_` or `DYLD_`. An adapter's own `*_env` setting
(`token_env`) names the key, never a `BEARING_` variable.
