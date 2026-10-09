# 9. Adapters run as sandboxed WebAssembly modules with granted capabilities

Date: 2026-09-29 · Status: accepted (with amendment), 2026-10-02

> Proposed 2026-09-29. Accepted 2026-10-02 after the
> [WASM adapters spike](https://github.com/dhm116/bearing/blob/spike/wasm-adapters/docs/spikes/wasm-adapters.md) (PR #24). Context,
> Decision, Shape and Consequences are the original proposal; where the
> amendment differs, the amendment wins, and the changed bullets say so.
> See [the amendment](#amendment-accepted-with-conditions-2026-10-02).
> A13 was revised on 2026-10-03: the stdio transport is retired, and
> adapters that can't run as WASM run as local processes on a Unix socket.
> On 2026-10-08, A6 gained the per-scheme host verifiers declared in the
> manifest, and A8 says Extism's own host functions are disabled.

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
| Adopt wasmCloud | Mature capability-provider model; WIT interfaces; distributed "lattice" over NATS | Its own runtime (Rust, wasmtime), scheduler and deployment model to operate; hard to fit a single Bearing binary; less control over what adapters can reach |

wasmCloud's central idea is worth keeping even without wasmCloud itself: the
sandbox exposes nothing but a small set of standard **capabilities**, each a
narrow interface, and an operator grants them per component. That gives
adapters a consistent surface to build against and gives Bearing one place
to implement shared behavior such as logging, tracing, caching and
credential handling.

## Decision

- **The adapter interface is a Protobuf service** (ADR 6): `Describe`,
  `Sync` (cursor paged) and `Handle` (webhook). Messages cross every
  boundary as Protobuf bytes.
  *Amended (A13): with no exception. Local-process adapters serve the same
  service over a Unix socket.*
- **The default runtime is WASM, hosted with Extism** on wazero (pure Go, no
  CGO). An adapter ships as one `.wasm` module plus a manifest.
  *Amended (A4): wazero stays; Extism versus a thin wazero ABI is decided on
  copy cost and attack surface.*
- **Adapters reach the world only through host capabilities.** Bearing
  defines a small, versioned set of capabilities. Each one is a Protobuf
  service (ADR 6) in `proto/bearing/capability/<name>/v1/`:

  | Capability | What it gives an adapter | What the host adds |
  | --- | --- | --- |
  | `http` | Outbound HTTP requests | Host allowlist; credentials injected from secret references, so the adapter never sees tokens; an `otelhttp` span; rate-limit handling |
  | `log` | Structured logs | Routed to `pkg/telemetry` with the adapter, source and trace attached |
  | `trace` | Child spans and attributes | Spans parented to the sync or webhook trace |
  | `metrics` | Counters and gauges declared in the manifest | Prefixed and labelled per adapter and source |
  | `config` | This source's typed settings (ADR 10) | Already validated at apply time |
  | `kv` | A small key-value cache (ETags, lookups) with TTLs | Scoped to one source; stored in the main store |
  | `clock` | Current time | Controllable in tests |

  An adapter calls every capability through one host function,
  `bearing_call(capability, method, request_bytes) -> response_bytes`. New
  capabilities need no new ABI.
- **Capabilities are granted, not assumed.** Access is deny-by-default at
  two levels:
  - The adapter's manifest declares which capabilities it needs and their
    scope, for example `http: [api.github.com]`, `kv`, `log`. A module that
    calls anything else gets a permission error.
  - Each `Source` (ADR 10) can narrow that further, but never widen it. For
    example, a source could use a GitHub Enterprise host or turn off `kv`.

  Bearing shows the effective grant before a source is applied, so an
  operator reviews exactly what an adapter will be able to do.
- **Local or remote, same interface.** In a single binary, capabilities are
  in-process host functions. In a distributed install, a capability can be
  served by a separate provider service over Connect, for example a shared
  HTTP egress proxy with fixed IPs or a shared cache. Adapters can't tell the
  difference, which keeps standalone and distributed installs one design.
- **Stay close to WASI.** Where a WASI 0.2 interface exists (`wasi:http`,
  `wasi:logging`, `wasi:keyvalue`, `wasi:config`), our capability follows its
  shape and names. If Go's component-model tooling matures, we can move to
  WIT components, and wasmCloud interoperability stays possible, without
  redesigning the capabilities.
- **Modules are pinned by digest** (sha256) in configuration (ADR 10), and
  can be signed and verified (Sigstore) before loading.
  *Amended (A11): first-party modules are embedded in the binary; external
  modules stay digest-pinned, with an optional signature check.*
- **A second runtime for what WASM can't do yet.** Remote adapters implement
  the same Protobuf service over Connect/gRPC and run as their own service.
  The core can't tell the difference. This covers adapters that need
  libraries that don't compile to WASM, or special network access.
  *Amended (A13): the same service also runs as a local process the core
  starts, on a Unix domain socket.*
- **The stdio JSON-RPC transport is retired.** Existing adapters (GitHub)
  are ported to WASM.
  *Amended (A13): stdio is retired as decided. WASM becomes the default
  runtime when [M4 (WASM adapters and capabilities)][M4] lands, and the
  GitHub adapter is ported in M4. Adapters that can't run as WASM run as
  local processes serving the Protobuf service on a Unix socket.*

## Shape

An adapter module sees one function, `bearing_call`. Everything behind it is
Bearing's, and every call is checked against the grant.

![An adapter module in the WASM sandbox calls one host function; a grant check allows http, local capabilities and kv, or returns a permission error; secrets are injected into http outside the module.](diagrams/adr9-sandbox.svg)

The same interfaces work standalone and distributed. Only where a capability
runs changes.

![Standalone, capabilities run in-process; distributed, local capabilities stay on the worker while http egress and the kv cache can be remote providers over Connect; a remote adapter service is the fallback runtime.](diagrams/adr9-topology.svg)

## Amendment: accepted with conditions (2026-10-02)

The [spike](https://github.com/dhm116/bearing/blob/spike/wasm-adapters/docs/spikes/wasm-adapters.md) ran the GitHub adapter's unchanged
source as a WASM module with output identical to the stdio adapter, and ran
the AWS SDK for Go v2 over the host `http` capability. It also found costs
the proposal did not assume. ADR 9 is accepted with these conditions; the
spike's "Security properties verified / not verified" section lists what is
still unproven. Control IDs refer to the
[threat model](../security/threat-model.md).

- **A1. Toolchain.** Upstream Go (`GOOS=wasip1`, `//go:wasmexport`,
  `-buildmode=c-shared`) is the supported Go toolchain. TinyGo is an
  optional size optimization: an adapter may ship a TinyGo build only if
  that build passes the adapter conformance suite and the hostile-guest
  module (A12). TinyGo could not build the unmodified adapter or the AWS
  SDK, and crashed at init with cel-go linked.
- **A2. Module size.** `pkg/telemetry` splits into an API part and a setup
  part (SDK, exporters). Adapters and `pkg/adapter` import only the API
  part, or the protocol types move to their own package. That alone took
  the Go module from 25.5 to 12.5 MiB. CI enforces a size budget per
  module.
- **A3. Precompile and cache.** A module is compiled when its Source is
  applied, or at first start for embedded modules, never on the sync path
  (cold compile is 7–14 s per upstream Go module). The persistent
  compilation cache holds native code, so it is protected under
  C-ADAPTER-12: local only, owned by the Bearing user with mode 0700, and
  keyed by wazero's own cache key plus the wazero version, never by a bare
  module digest.
- **A4. ABI.** The ABI is chosen on copy cost **and** attack surface before
  the guest SDK is frozen: Extism with caller-memory or bulk-copy access
  added upstream, or a thin wazero ABI. Extism's go-pdk copies 8 bytes per
  host call (about 0.1–0.19 s per MiB crossed; the lean adapter synced in
  ~850 ms on Extism against ~600 ms on the raw ABI), and Extism adds its own
  kernel imports, dependencies and glob host matching. The thin ABI imports
  only `bearing_call`, a read helper and the WASI profile (A8). Either way
  the shape `bearing_call(capability, method, request_bytes) ->
  response_bytes` stays, and the SDKs hide the choice from adapter code.
  SDK clients surface a permission error to the adapter and never fall
  back silently (the spike's `clock` client fell back to the WASI clock).
- **A5. Messages.** Capability and adapter messages are Protobuf, with HTTP
  bodies as `bytes` fields, never base64 inside JSON. protobuf-go adds a
  measured +3.6 MiB (+29%) to the lean Go module; its CPU cost on `wasip1`
  is measured before the guest SDK is frozen.
- **A6. Credentials stay on the host.** No credential enters a guest
  (C-SECRET-3, C-ADAPTER-6):
  - Bearer tokens are injected by the host, bound to one host.
  - AWS SigV4 is signed by the host, which strips any guest
    `Authorization` and `X-Amz-*` headers and computes the payload hash
    itself.
  - Webhook deliveries are verified by the host, before any parsing
    (C-INGEST-2, C-INGEST-9). `Handle` receives only authenticated
    deliveries. There is no HMAC capability. The host has one verifier per
    signature scheme; the manifest declares the scheme and signature
    headers (never a secret), and the Source supplies the secret by
    reference (ADR 7). This supersedes in part
    [ADR 3](0003-adapter-protocol.md) and the adapter protocol spec, which
    put verification in the adapter.
- **A7. Validation on the host.** The host validates every observation
  (C-GEN-3, C-ADAPTER-7). Guests link no protovalidate or cel-go, which
  added 13.8 MiB to the Go module.
- **A8. WASI preview1 profile.** Every Go-built module imports WASI
  preview1, so "no WASI" (the original C-ADAPTER-1) cannot be met. Modules
  get a fixed profile instead: empty args and env, no preopens, no sockets
  (TinyGo's `sock_*` imports are stubs that return an error), stdout and
  stderr owned by the host and discarded or bounded and rate-limited,
  `crypto/rand` as the random source, a fake wall clock (real time comes
  only through `clock`) and a real monotonic clock. Go's runtime and GC
  must be verified under the fake wall clock (an A12 case) before the
  profile is final. Any import outside the profile fails the load. If
  Extism is used, its own HTTP, config, var and path host functions are
  disabled (`allowed_hosts` and `allowed_paths` empty), so a guest reaches
  the network, configuration and files only through `bearing_call` and its
  grant (C-ADAPTER-1), and Bearing refuses to start with
  `EXTISM_ENABLE_WASI_OUTPUT` set.
- **A9. Limits.** Every guest call runs under a deadline with wazero's
  `CloseOnContextDone` and a memory cap of 64–128 MiB per instance, counted
  across all of its memories (Extism instances have two), plus size caps on
  capability requests, responses and observations. Any breach aborts the
  call and discards the instance (C-ADAPTER-3). The memory cap cost nothing
  measurable; `CloseOnContextDone` roughly doubles sync time (about 2× guest
  CPU; 1.3–1.6 s for the spike's 456-request sync). Worker pools are sized
  for that and re-measured under load with realistic API latency.
- **A10. `http` capability rules** (C-ADAPTER-4, C-ADAPTER-5, C-ADAPTER-6):
  HTTPS only, port 443 unless the grant names a port; exact hostnames,
  canonicalized (lowercase, IDNA to punycode, no trailing dot); GET and
  HEAD unless granted; a dialer that
  checks the resolved IP and dials the address it checked; every redirect
  re-checked, at most 5; `Proxy: nil`, so environment proxy settings are
  ignored; guest-set `Authorization`, `Cookie`, `Proxy-*`, `Host` and
  `X-Amz-*` stripped; an oversize response is an error, not a truncation.
- **A11. Packaging** (C-SUPPLY-6, C-ADAPTER-9). First-party modules are
  built from this repository in the same build and embedded
  gzip-compressed in the one `bearing` binary, with the sha256 recorded at
  build time and checked before compile. They are never downloaded. A
  `noadapters` build tag exists from the start, so a `bearing-core` package
  costs one CI job when it is needed. External modules load only by sha256
  digest. Three upstream Go adapters put the binary at about 38–42 MiB.
- **A12. Hostile-guest conformance module.** A test module tries
  `environ_get`, `path_open`, an unknown capability, an ungranted method,
  its own `Authorization`, a cross-host redirect, a pure CPU loop (also
  against a warm compile cache), memory growth and an output flood. It also
  checks that the Go runtime and GC run correctly under the profile's fake
  wall clock (A8), and that a denied capability reaches the adapter as a
  permission error with no silent fallback (A4). Every
  runtime and capability provider must pass it before any adapter is
  enabled by default.
- **A13. WASM by default; local processes on a Unix socket for the rest;
  stdio retired** (revised 2026-10-03). WASM becomes the default adapter
  runtime when [M4][M4] lands, and the GitHub adapter is ported to WASM in
  M4 (A11). New first-party adapters target WASM.
  - An adapter that can't run as WASM (for example, one written in a
    language without a mature `wasip1` toolchain) runs as a **local
    process** the core starts. It serves the same Protobuf adapter service
    that remote adapters serve (Connect/gRPC), on a Unix domain socket. The
    core can't tell a local adapter from a remote one except by how it was
    started. Prior art: HashiCorp go-plugin.
  - The stdio JSON-RPC transport (ADR 3) is retired. No users depend on it,
    and the initial scaffold need not be preserved. The scaffold's stdio
    code runs the GitHub adapter until M4 replaces it; nothing new is built
    on it. Protobuf is the only source of truth for adapter messages
    (ADR 6), with no JSON-RPC exception.
  - The socket is protected (threat model C-ADAPTER-13 to 15). It lives
    in a private directory (mode 0700) that the core creates per adapter
    instance inside a verified runtime directory. The core passes its
    path to the child in one place, the `BEARING_ADAPTER_SOCKET`
    environment variable, and the started process must listen on it
    itself. On every connection the core checks that the peer is the
    child it started: the UID must be the one the core started the child
    as (`SO_PEERCRED` on Linux, `getpeereid` on the BSDs and macOS), and
    the PID must be the child's where the OS reports one (`SO_PEERCRED` on
    Linux, `LOCAL_PEERPID` on macOS; `getpeereid` gives the UID only).
    The core removes the socket and its directory when the adapter stops.
  - The core owns the child (C-ADAPTER-16 to 19): a minimal environment
    with no secrets, secret references or exporter settings
    ([#25](https://github.com/dhm116/bearing/issues/25)); core-side limits
    in place of the sandbox's (an RPC deadline, a start-up deadline, a
    response size cap, per-page caps on observations, and bounded,
    rate-limited output); and a lifecycle that kills the process group
    when the child stops, while the child dies with the core (`Pdeathsig`,
    pipe EOF).
  - A local-process adapter is configured by absolute path plus sha256;
    the core verifies the digest from an open file descriptor and, on
    Linux, executes that descriptor (C-ADAPTER-22). The sha256 field is
    part of the M4 Source and Adapter configuration.
  - The core resolves the Source's secrets and sends their values to the
    child once, over an inherited pipe or a `Configure` RPC, never in the
    environment (C-ADAPTER-20, C-SECRET-3).
  - stdout is no longer reserved for the protocol. The child's logs and
    telemetry go to the core, which captures stdout and stderr (or serves
    an OTLP receiver in the private directory), tags them with adapter and
    Source, rate-limits them and forwards them (C-ADAPTER-18).
  - A local-process adapter is not sandboxed. Running as the service user,
    it is equivalent to the host: grants, output validation, Source
    scoping, the minimal environment and C-SECRET-3's "no others" limit
    what a correct adapter receives and what the core accepts, not what a
    malicious one can do. Operators run only local-process adapters they
    would run as the service user. The sandbox controls in threat model B2
    don't apply, apart from C-ADAPTER-3's limits, which the core enforces
    from its side. Running each local-process adapter under its own
    unprivileged UID is a planned hardening option; the core's start
    contract is written against "the UID the core started the child as",
    so it can be added without changing the SDK.
  - Windows has `AF_UNIX` from Windows 10, but the core refuses to start
    local-process adapters there until a Windows control (an ACL on the
    directory, `SIO_AF_UNIX_GETPEERPID`) is specified and tested
    (C-ADAPTER-21).
- **A14. AWS.** The AWS SDK for Go v2 works in a Go module over the `http`
  capability (STS: 13.5 MiB). The remote runtime stays as a fallback, but
  AWS does not need it on technical grounds.
- **A15. Transition rule for webhooks and secrets.** Host verification is
  tied to ingest, not to M4. Ingest stays off (C-INGEST-1) until the host
  verifier exists; the host verifier ships with the first ingest
  transport, whichever milestone that is. Until then an adapter's
  `Handle` is reached only through local or test paths, and the adapter's
  own verification, as the adapter protocol spec requires, is the check.
  Once ingest is on, the host verifies every delivery before it is logged
  (ADR 7), for every runtime (A6); a local-process adapter may still
  verify as defense in depth. Local-process adapters still receive their
  own Source's credentials, an exception to C-SECRET-3 that holds only for
  local processes, and only for the secrets that Source names; the core
  resolves them and sends the values (A13).
  `docs/spec/adapter-protocol.md` says now that its stdio transport is
  retired; rewriting it for the Protobuf service and bumping
  `adapter.ProtocolVersion` happen in [M4][M4].

## Consequences

- **Spike first** (done, see the amendment): build the GitHub adapter as
  a WASM module, and check the AWS SDK for Go v2 against the host HTTP
  function (a custom `http.RoundTripper`). If the AWS SDK can't be made to
  work, the AWS adapter uses the remote runtime. The decision doesn't
  depend on it.
- WASM doesn't make Bearing scale by itself. It helps because adapters
  become cheap, stateless functions that any worker can run, so scale comes
  from the event log and worker pool (ADR 7).
- Transports narrow rather than widen: adapters can use only the capabilities
  the host offers. A new transport or feature is added once, as a capability,
  and every adapter that is granted it can use it.
- Shared behavior lives in capabilities, not in each adapter: retries,
  rate limiting, caching, credentials and telemetry are written once in Go
  and behave the same for every adapter in every language.
- Bearing publishes a small SDK per language (Go and Rust first) that wraps
  `bearing_call` with typed clients generated from the capability protos,
  built on the Extism PDKs. *Amended (A4): on whichever ABI is chosen.*
- Each capability gets a conformance suite, so a remote provider behaves the
  same as the in-process one.
- Supersedes [ADR 3](0003-adapter-protocol.md) and
  `docs/spec/adapter-protocol.md` once accepted. *Amended (A6, A13, A15):
  supersedes them in part. The stdio transport is retired; WASM becomes
  the default runtime in M4, with local-process adapters on a Unix socket
  for the rest; and webhook verification moves to the host with the first
  ingest transport. ADR 3's stateless, cursor-paged, read-only adapter
  model carries over.*
- Per sync, WASM costs 3–5× the stdio process's wall time in the spike, and
  6.5–8.5× with `CloseOnContextDone` on, mostly guest CPU. Against real
  APIs, request latency still dominates.

[M4]: https://github.com/dhm116/bearing/milestone/5
