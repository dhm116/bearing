# Spike: WASM adapters

Date: 2026-10-02 · Issue: dhm116/bearing#7 · Decides: [ADR 9](../adr/0009-wasm-adapters.md)

**Question.** Can the GitHub adapter run as a WASM module on an Extism host
(go-sdk on wazero) that reaches the network only through one host function,
`bearing_call`, and what does that cost?

**Answer.** Yes. The unmodified `adapters/github` package runs in a WASM
module, and its observations match the stdio adapter's exactly. Three things
cost more than ADR 9 assumes: module size (because of `pkg/telemetry`),
Extism's memory ABI, and credentials that aren't bearer tokens.
Recommendation: **go, with conditions** (see the end of this report).

## Setup

Code is in [`spikes/wasm/`](../../spikes/wasm/). Each part has its own
`go.mod` with `replace bearing.example => ../../..`, so the root module and
its dependencies are untouched.

| Part | What it is |
| --- | --- |
| `guest/github` | Imports `bearing.example/adapters/github` unchanged. It injects `HTTP` (a `RoundTripper` over the `http` capability), `Now` (the `clock` capability) and an empty `Getenv`. The host injects the token, so the guest never sees it. |
| `guest/githublean` | The same adapter code as a generated copy (`make lean`), with `pkg/telemetry` swapped for an API-only shim that logs through the `log` capability. It also copies `pkg/adapter/protocol.go`, because `pkg/adapter` imports `pkg/telemetry`. Logic is byte-for-byte the same. |
| `guest/githubraw` | The lean adapter on a ~100-line raw wazero ABI (`guest/rawabi`) instead of Extism. It isolates Extism's cost. |
| `guest/capability`, `guest/bearingcall` | A sketch of the guest SDK: typed `http`/`clock` clients over `bearing_call(capability, method, request)` on the Extism ABI. |
| `aws` | AWS SDK for Go v2: one `sts:GetCallerIdentity` call through the same `RoundTripper`. |
| `host` | The harness. It loads modules with a wazero compilation cache and serves `bearing_call` (`http` with host allowlist, method allowlist and token injection; `clock`; `log`). It replays fixtures from an `httptest` server, runs full syncs with host-side `model.Validate`, and compares the result with `bin/bearing-adapter-github` over stdio. It also measures everything below and builds the binary-size probes. |

Run it with `make -C spikes/wasm TINYGO=… WASM_OPT=…` (targets: `modules`,
`opt`, `bench`).

**Fixtures.** Neither api.github.com nor github.com is reachable from the
spike environment (the egress proxy returns 403), so the "recording" is
generated. `host/fixtures.go` builds full REST 2022-11-28 response objects
for an org with:

- 230 repositories, served as 3 pages of 100;
- 14 nested teams, one of them with 130 members (2 member pages);
- CODEOWNERS files in each of GitHub's three locations, and repositories with none.

That is 180 responses (1.8 MB). One sync makes 456 requests and emits 777
observations. The fixture server checks the token, API version and raw
media type, and answers unknown paths with GitHub's 404 body. The fixtures
are stored in `host/testdata/github-fixtures.json.gz`.

**Versions.** Go 1.27.1, TinyGo 0.42.0 (LLVM 22.1.4, taken from the
`tinygo/tinygo:0.42.0` image), `github.com/extism/go-sdk` v1.7.1,
`github.com/extism/go-pdk` v1.1.3, `github.com/tetratelabs/wazero` v1.12.0
(go-sdk asks for v1.9.0; v1.12.0 worked unchanged), binaryen `wasm-opt`
108 (`-Oz`), AWS SDK `service/sts` v1.51.1, `buf.build/go/protovalidate`
v1.4.0 with `cel.dev/cel-go` v0.32.0. Hardware: linux/amd64, 4 vCPU Xeon @
2.8 GHz, 15 GB. Timings are medians of 7 runs.

## Results

Full sync = a new instance from the compiled module, then every page. "In
host fn" is time inside `bearing_call`; nearly all of it is the HTTP round
trip to the fixture server, which the stdio adapter pays too.

| Runtime | Module | Size (gzip) | Cold compile | Warm compile | Instantiate + first call | Full sync | Guest memory peak | Matches stdio |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| stdio process | `bearing-adapter-github` | 23.0 MiB | n/a | n/a | n/a | **190 ms** | 28.2 MiB RSS | reference |
| native, in-process | (linked) | n/a | n/a | n/a | n/a | 97 ms | n/a | n/a |
| Extism, Go | `github` (unmodified) | 25.5 MiB (5.6) | 13.9 s | 487 ms | 30 ms | 884 ms | 20.5 MiB | yes |
| Extism, Go | `github` + wasm-opt | 23.5 MiB | 11.6 s | 483 ms | 34 ms | 885 ms | 20.5 MiB | yes |
| Extism, TinyGo | `github` (unmodified) | does not compile | | | | | | |
| Extism, Go | `githublean` | 12.5 MiB (3.3) | 6.5 s | 246 ms | 19 ms | 850 ms | 14.0 MiB | yes |
| Extism, TinyGo | `githublean` | **2.0 MiB** | 2.0 s | 54 ms | 2.6 ms | 923 ms | 24.0 MiB | yes |
| Extism, TinyGo | `githublean` + wasm-opt | 1.9 MiB (0.7) | 2.1 s | 46 ms | 2.4 ms | 974 ms | 24.0 MiB | yes |
| raw wazero, Go | `githubraw` | 12.5 MiB | 7.1 s | 239 ms | 18 ms | 601 ms | 14.0 MiB | yes |
| raw wazero, TinyGo | `githubraw` + wasm-opt | 1.9 MiB | 2.3 s | 48 ms | 3.1 ms | **543 ms** | 24.0 MiB | yes |
| Extism, Go | `githublean` + protovalidate | 26.3 MiB | 13.5 s | 452 ms | 52 ms | 951 ms | 20.5 MiB | yes |
| Extism, TinyGo | `githublean` + protovalidate | 6.7 MiB | | | crashes in init | | | |

In every WASM row, `bearing_call` handled 1,233 calls per sync: 456 `http`
calls and 777 `clock` calls. 105 KiB went to the host and 1.8 MiB came back.
About 170–220 ms of each sync was spent in the host function, nearly all of
it in the HTTP capability. With a module granted only the fixture host, a
sync pointed at `api.evil.example` failed with "permission denied" in every
variant.

AWS SDK v2 (`aws` module, upstream Go only):

| Module | Size | Anonymous (signing left to host) | SigV4 in guest (dummy keys) |
| --- | ---: | --- | --- |
| `aws-sts` | 13.5 MiB (3.4 gzip) | works, 26 ms | works, 29 ms, `AWS4-HMAC-SHA256` header reached the stub |
| `aws-sts`, TinyGo | does not compile | | |

Binary size (`go build`, linux/amd64):

| Binary | Default | Stripped |
| --- | ---: | ---: |
| `bearing` today | 22.9 MiB | 15.6 MiB |
| + Extism/wazero host | 28.1 MiB (+5.2) | 19.1 MiB (+3.5) |
| + host + embedded TinyGo lean module | 29.9 MiB | 21.0 MiB |
| + host + embedded Go unmodified module | 53.6 MiB | 44.7 MiB |

## Findings

1. **The adapter code ports as-is.** Upstream Go `wasip1` with
   `//go:wasmexport` and `-buildmode=c-shared` builds the package unchanged.
   Adapters already inject `HTTP`, `Now` and `Getenv`, which made this a
   three-field swap. The output was identical in every variant that ran.
2. **`pkg/telemetry` sets the module size.** The adapter imports
   `pkg/telemetry`, and so does `pkg/adapter`. That pulls in the OTel SDK,
   the OTLP exporters, gRPC and protobuf: 25.5 MiB with upstream Go. With an
   API-only telemetry package, the module is 12.5 MiB with upstream Go and
   2.0 MiB with TinyGo. The rest of the upstream Go size is mostly
   `net/http` and `crypto/tls`, linked in although the guest never dials.
   `wasm-opt -Oz` saves only 5–9%.
3. **TinyGo is fragile.**
   - It can't compile the unmodified adapter: `golang.org/x/net/http2`,
     which the OTLP exporters pull in, uses Go 1.27 `net/http` APIs that
     TinyGo's `net/http` lacks.
   - It can't compile the AWS SDK: `net.OpError.Temporary` is missing.
   - With cel-go linked, the module compiles but crashes at init (out of
     bounds in `runtime.hashmapGet`).
   - Its heap peaks higher (24 MiB vs 14 MiB), and the lean build took
     about 2 minutes.

   Where it works, it is 6× smaller and compiles 3–5× faster. Upstream Go
   compiled everything we tried.
4. **Compile time is per module, so caching is mandatory.** wazero's
   ahead-of-time compile takes 2 s for the TinyGo module and 7–14 s for the
   upstream Go modules, roughly linear in size. With the on-disk cache it
   takes 50–490 ms. Instantiation plus Go runtime init takes 2–50 ms, so a
   fresh instance per sync is affordable.
5. **Extism's memory ABI is the largest avoidable cost.** go-pdk moves
   bytes between guest and kernel memory with one host call per 8 bytes
   (`extism_load_u64`/`extism_store_u64`). The same lean adapter synced in
   ~850–970 ms on Extism and ~540–600 ms on the raw ABI, about 0.1 s per
   MiB that crosses the boundary. Extism host functions get no handle to
   the caller's memory (`api.GoFunc`), so `bearing_call` can't copy
   directly without an upstream change or our own host.
6. **Per sync, WASM is 3–5× slower than the stdio process here.** That is
   190 ms for stdio against 540–970 ms for WASM. With the copies removed,
   the remaining ~350 ms is guest CPU: `encoding/json` over 1.8 MiB runs
   about 10× slower in Go-on-wasm than natively. Encoding HTTP bodies as
   base64 inside JSON cost more again; the final runs use a binary frame,
   as a Protobuf `bytes` field would. Against the real API, 456 requests at
   tens of ms each dominate. The CPU cost matters for how many workers we
   need, not for sync latency.
7. **Validation stays on the host.** The harness validated every
   observation host-side with `model.Validate`; the guest needs no
   validator. Linking protovalidate and cel-go into the guest adds
   13.8 MiB to the upstream Go module and 4.7 MiB to the TinyGo module, and
   the TinyGo build crashes.
8. **Credentials: bearer tokens work; HMAC and SigV4 don't yet.** These
   contradict ADR 9.
   - **Bearer tokens.** The host injected the GitHub token, and the guest
     never saw it.
   - **Webhooks.** `Handle` reads the webhook secret through `Getenv`,
     which the guest doesn't have, so webhook verification must move to
     the host or behind a capability. ADR 3 places it in the adapter.
   - **AWS.** The SDK works over the `RoundTripper` both unsigned and
     SigV4-signed in the guest. Signing in the guest, though, puts AWS keys
     in the guest. To keep the adapter from seeing credentials, the `http`
     capability needs a SigV4 credential mode that signs on the host.
9. **The AWS SDK is feasible with upstream Go.** STS alone is a 13.5 MiB
   module (3.4 MiB gzip). ADR 9's remote-runtime fallback isn't needed for
   AWS on technical grounds.
10. **The host costs +5.2 MiB (+23%) in `bearing`.** Embedding a TinyGo
    module adds about its own size. An upstream Go module adds 25 MiB.

## Recommendation

**Go**: accept ADR 9 with WASM as the default adapter runtime and a single
`bearing` binary that includes the host. There is no case for a separate
`bearing-core` on size: the host costs 3.5–5.2 MiB, and modules are loaded
by digest rather than embedded. The conditions:

1. Split `pkg/telemetry` into an API part (what adapters import) and a setup
   part (what binaries import). Move the protocol types out of
   `pkg/adapter`; ADR 6's generated Protobuf types can replace them. Add a
   CI size budget per module.
2. Use upstream Go (`wasip1`, `go:wasmexport`) as the supported Go
   toolchain. Treat TinyGo as an optional size optimization that each
   adapter must prove with the conformance suite.
3. Compile modules when a Source is applied and keep a persistent wazero
   compilation cache keyed by module digest. Never compile on the sync
   path.
4. Fix the ABI copy cost before adapters page large payloads. Either get
   Extism host functions access to caller memory (or a bulk copy)
   upstream, or keep `bearing_call` on our own thin wazero ABI, which the
   raw prototype showed is small. In both cases, keep `bearing_call`'s
   shape so the decision stays reversible.
5. Messages are Protobuf with `bytes` bodies, never JSON with base64.
6. Credentials never enter the guest. Add host-side webhook signature
   verification (or an `hmac.verify`-style capability) and a SigV4 mode for
   the `http` capability before the AWS adapter, and amend ADR 3/9 to say
   so.
7. Validation stays on the host. Guests link no protovalidate or cel-go.
8. Before relying on throughput, measure worker CPU under load with
   realistic upstream latency. The 3–5× CPU overhead per sync is
   acceptable for API-bound syncs but sets worker counts.
