# 9. Adapters run as sandboxed WebAssembly modules

Date: 2026-09-29 · Status: proposed

## Context

[ADR 3](0003-adapter-protocol.md) made each adapter a separate executable
speaking JSON-RPC on stdio. That keeps adapters independent of the core's
language, but:

- An adapter process can do anything its OS user can: read files, open any
  network connection, run programs. Operators have to trust every adapter
  binary fully.
- Running an adapter means managing a child process per adapter per host,
  which fits poorly with an always-on, horizontally scaled core (ADR 7).
- Distributing adapters means per-platform binaries.

| Option | Pros | Cons |
| --- | --- | --- |
| Processes on stdio (today) | Any language, any library | No sandbox; process management; per-platform builds |
| WASM via Extism (wazero, pure Go) | Sandbox by default; one portable `.wasm` per adapter; runs in-process, so any worker can run any adapter; no CGO | Network only through host functions; some SDKs don't compile to WASM yet; Go adapters need TinyGo or Go's `wasip1` target |
| WASM via wasmtime (component model, WASI 0.2) | Richest standard (`wasi:http`) | CGO; the Go tooling for components is immature |
| Remote adapters over gRPC | Any language, any library, scales separately | The operator runs and secures another service |

## Decision

- **The adapter interface is a Protobuf service** (ADR 6): `Describe`,
  `Sync` (cursor paged) and `Handle` (webhook). Messages cross every
  boundary as Protobuf bytes.
- **The default runtime is WASM, hosted with Extism** on wazero (pure Go, no
  CGO). An adapter ships as one `.wasm` module plus a manifest.
- **The sandbox is the permission model.** The manifest declares the hosts
  the adapter may call (`api.github.com`) and the secrets it needs by name.
  The host enforces both. An adapter has no filesystem, no clock beyond what
  the host gives it, and no other network access. Outbound HTTP goes
  through a host function that applies the allowlist, injects credentials
  and records an `otelhttp` span.
- **Modules are pinned by digest** (sha256) in configuration (ADR 10), and
  can be signed and verified (Sigstore) before loading.
- **A second runtime for what WASM can't do yet.** Remote adapters implement
  the same Protobuf service over Connect/gRPC and run as their own service.
  The core can't tell the difference. This covers adapters that need
  libraries that don't compile to WASM, or special network access.
- **The stdio JSON-RPC transport is retired.** Existing adapters (GitHub)
  are ported to WASM.

## Consequences

- **Spike first:** build the GitHub adapter as a WASM module, and check the
  AWS SDK for Go v2 against the host HTTP function (a custom
  `http.RoundTripper`). If the AWS SDK can't be made to work, the AWS
  adapter uses the remote runtime. The decision doesn't depend on it.
- WASM doesn't make Bearing scale by itself. It helps because adapters
  become cheap, stateless functions that any worker can run, so scale comes
  from the event log and worker pool (ADR 7).
- Transports narrow rather than widen: adapters can use only what the host
  offers (HTTP at first). New transports are added once, in the host, for
  every adapter.
- Adapter authors in Rust, Go (TinyGo or `wasip1`), JavaScript, Python and
  other languages use the Extism PDKs.
- Supersedes [ADR 3](0003-adapter-protocol.md) and
  `docs/spec/adapter-protocol.md` once accepted.
