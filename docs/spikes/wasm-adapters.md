# Spike: WASM adapters

Date: 2026-10-02 · Issue: dhm116/bearing#7 · Decides: [ADR 9](../adr/0009-wasm-adapters.md)
· Controls cited: [threat model](../security/threat-model.md)

**Question.** Can the GitHub adapter run as a WASM module on an Extism host
(go-sdk on wazero) that reaches the network only through one host function,
`bearing_call`, and what does that cost?

**Answer.** Yes. The adapter's source runs in a WASM module without
changes, and its observations match the stdio adapter's exactly. Three
things cost more than ADR 9 and the threat model assume:

- **Module size.** Each first-party module embedded in the binary (as
  C-SUPPLY-6 requires) adds 12.5–16 MiB with upstream Go.
- **Interruptible execution.** Turning on the wazero setting that lets a
  deadline stop guest code roughly doubles sync time.
- **Credentials that aren't bearer tokens.** Webhook HMAC and AWS SigV4
  don't work without host-side support.

Also, every Go-built module imports WASI preview1, so C-ADAPTER-1 can't be
met as written. Recommendation: **go, with conditions** (see the end of
this report).

## Setup

Code is in [`spikes/wasm/`](../../spikes/wasm/). Each part has its own
`go.mod` with `replace bearing.example => ../../..`. The root module and its
`go.mod` are untouched. MVS in a spike module can pick different versions of
shared dependencies than the root module does, so "unchanged adapter" here
means unchanged *source*, not an identical dependency graph.

| Part | What it is |
| --- | --- |
| `guest/github` | Imports `bearing.example/adapters/github` unchanged. It injects `HTTP` (a `RoundTripper` over the `http` capability), `Now` (the `clock` capability) and an empty `Getenv`. The host injects the token. |
| `guest/githublean` | The same adapter source as a generated copy (`make lean`), with `pkg/telemetry` swapped for an API-only shim that logs through the `log` capability. It also copies `pkg/adapter/protocol.go`, because `pkg/adapter` imports `pkg/telemetry`. |
| `guest/githubraw` | The lean adapter on a ~100-line raw wazero ABI (`guest/rawabi`) instead of Extism. It isolates Extism's cost. |
| `guest/capability`, `guest/bearingcall` | A sketch of the guest SDK: typed `http`/`clock` clients over `bearing_call(capability, method, request)`. |
| `aws` | AWS SDK for Go v2: one `sts:GetCallerIdentity` call through the same `RoundTripper`. |
| `host` | The harness: wazero compilation cache; `bearing_call` serving `http` (host and method allowlist, token injection), `clock` and `log`; full syncs with host-side `model.Validate`; comparison with `bin/bearing-adapter-github` over stdio; the measurements below. `-limits` re-runs each module under a memory cap, `CloseOnContextDone` and a per-sync deadline. |

Run it with `make -C spikes/wasm TINYGO=… WASM_OPT=…` (targets: `modules`,
`opt`, `bench`). For the limits run:
`cd spikes/wasm/host && go run . -limits -only 'github-lean.go.wasm,raw-github-lean.go.wasm'`.

**Fixtures.** Neither api.github.com nor github.com is reachable from the
spike environment (the egress proxy returns 403), so the "recording" is
generated. `host/fixtures.go` builds full REST 2022-11-28 response objects
for an org with:

- 230 repositories, served as 3 pages of 100;
- 14 nested teams, one of them with 130 members (2 member pages);
- CODEOWNERS files in each of GitHub's three locations, and repositories with none.

That is 180 responses (1.8 MB). One sync makes 456 requests and emits 777
observations.

**Versions.** Go 1.27.1, TinyGo 0.42.0 (LLVM 22.1.4, taken from the
`tinygo/tinygo:0.42.0` image), `github.com/extism/go-sdk` v1.7.1,
`github.com/extism/go-pdk` v1.1.3, `github.com/tetratelabs/wazero` v1.12.0
(go-sdk asks for v1.9.0; v1.12.0 worked unchanged), binaryen `wasm-opt`
108 (`-Oz`), AWS SDK `service/sts` v1.51.1, `buf.build/go/protovalidate`
v1.4.0 with `cel.dev/cel-go` v0.32.0. Hardware: linux/amd64, 4 vCPU Xeon @
2.8 GHz, 15 GB.

**Sampling.** Full-sync times are medians of 7 runs (9 in the limits run).
The cold-compile, warm-compile and instantiate columns are **single
samples**. The Go reviewer's rerun measured 948 ms warm compile where this
table says 487 ms, so treat those columns as ±2×.

## Results

Full sync = a new instance from the compiled module, then every page.
Roughly 170–220 ms of every WASM sync is the HTTP round trip to the fixture
server inside `bearing_call`, which the stdio adapter pays too.

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
With a module granted only the fixture host, a sync pointed at
`api.evil.example` failed with "permission denied" in every variant.

**Limits (B3 rerun).** These are the same lean modules, rebuilt after the
merge of `main`, with a 2048-page (128 MiB) memory cap, `CloseOnContextDone`
on, and a 30 s deadline on each sync. Each module ran without and then with
limits in one process; medians of 9. The last two columns run each limit on
its own:

| Module | Full sync, no limits | Both limits | Overhead | Memory cap only | `CloseOnContextDone` + deadline only |
| --- | ---: | ---: | ---: | ---: | ---: |
| Extism, Go lean | 784 ms | 1,616 ms | **+106%** | 778 → 728 ms (noise) | 796 → 1,587 ms (+99%) |
| raw wazero, Go lean | 576 ms | 1,260 ms | **+119%** | 559 → 529 ms (noise) | 608 → 1,284 ms (+111%) |
| Extism, TinyGo lean | 940 ms | 2,273 ms | +142% | not run | not run |
| raw wazero, TinyGo lean | 543 ms | 1,530 ms | +182% | not run | not run |

TinyGo was still installed, so its lean modules were rebuilt and included in
the combined run. The memory cap costs nothing measurable. All of the
overhead comes from `CloseOnContextDone`, which compiles a termination check
into the generated code. Guest CPU, measured as sync time minus host time,
grows about 2.3–2.7× with it.

The limits are enforced:

- A 50 ms deadline stopped every module with
  `module closed with context deadline exceeded`. Control came back within
  tens of ms of the deadline; the timer also covered the warm compile, so
  the overrun wasn't measured precisely.
- A 10 MiB cap made every module fail its first page instead of growing.
  The Go runtime hit `fatal error: out of memory` and the trap reached the
  host as an error.

The guest's runtime wrote that fatal message to the stderr the harness gave
it. That is what N6 warns about: the host must own the guest's output.

AWS SDK v2 (`aws` module, upstream Go only):

| Module | Size | Anonymous (signing left to host) | SigV4 in guest (dummy keys) |
| --- | ---: | --- | --- |
| `aws-sts` | 13.5 MiB (3.4 gzip) | works, 26 ms | works, 29 ms (**feasibility only**, see finding 8) |
| `aws-sts`, TinyGo | does not compile | | |

Binary size (`go build`, linux/amd64):

| Binary | Default | Stripped |
| --- | ---: | ---: |
| `bearing` today | 22.9 MiB | 15.6 MiB |
| + Extism/wazero host | 28.1 MiB (+5.2) | 19.1 MiB (+3.5) |
| + host + embedded TinyGo lean module | 29.9 MiB | 21.0 MiB |
| + host + embedded Go unmodified module | 53.6 MiB | 44.7 MiB |

## Findings

1. **The adapter source ports as-is.** Upstream Go `wasip1` with
   `//go:wasmexport` and `-buildmode=c-shared` builds the package
   unchanged. Adapters already inject `HTTP`, `Now` and `Getenv`, which
   made this a three-field swap. The output was identical in every variant
   that ran.
2. **`pkg/telemetry` sets the module size.** The adapter imports
   `pkg/telemetry`, and so does `pkg/adapter`. That pulls in the OTel SDK,
   the OTLP exporters, gRPC and protobuf: 25.5 MiB with upstream Go. With
   an API-only telemetry package, the module is 12.5 MiB with upstream Go
   and 2.0 MiB with TinyGo. Two fixes are needed:
   - Adapters, and `pkg/adapter`, must import only the telemetry API part,
     or the protocol types must move to their own package.
   - The rest of the upstream Go size is mostly `net/http` and
     `crypto/tls`, linked in although the guest never dials.

   `wasm-opt -Oz` saves only 5–9%.
3. **Protobuf in the guest isn't measured here.** The guest never linked
   protobuf-go. The Go reviewer measured +3.6 MiB (+29%) for it on the lean
   Go module (13.1 → 16.9 MB). Generated-message size and encode/decode CPU
   on wasip1 and TinyGo are still unmeasured; that is a follow-up before
   the guest SDK is frozen.
4. **TinyGo is fragile.**
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
5. **Compile time is per module, so caching is mandatory.** wazero's
   ahead-of-time compile takes about 2 s for the TinyGo module and 7–14 s
   for the upstream Go modules (single samples). With the on-disk cache it
   takes 50–950 ms. Instantiation plus Go runtime init takes 2–60 ms. The
   cache holds native code, which makes it a security asset (see the
   amendment list).
6. **Extism's memory ABI is the largest avoidable cost.** go-pdk moves
   bytes between guest and kernel memory with one host call per 8 bytes
   (`extism_load_u64`/`extism_store_u64`). That costs about 0.10–0.13 s
   per MiB crossed with upstream Go and 0.17–0.19 s with TinyGo; the lean
   adapter synced in ~850 ms on Extism against ~600 ms on the raw ABI.
   Extism host functions get no handle to the caller's memory
   (`api.GoFunc`), so `bearing_call` can't copy directly without an
   upstream change or our own host. Extism also links its own
   `extism:host/env` functions and extra dependencies (observe-sdk), and
   its `allowed_hosts` uses globs rather than exact names. Both widen the
   attack surface; the raw ABI imports only `bearing_call`, `bearing_read`
   and WASI.
7. **Per sync, WASM is 3–5× slower than the stdio process here, without
   limits.** That is 190 ms for stdio against 540–970 ms for WASM. Guest
   CPU is about 6–8× native. The 10× `encoding/json` figure is an
   inference; an `encoding/json` microbenchmark measures about 7×. Base64
   HTTP bodies inside JSON cost more again, so the final runs use a binary
   frame, as a Protobuf `bytes` field would. With `CloseOnContextDone` on,
   which C-ADAPTER-3 needs, sync time doubles again (1.3–1.6 s). Against
   the real API, 456 requests at tens of ms each still dominate latency,
   but a sync now takes 6.5–8.5× the stdio adapter's wall time here, most
   of it guest CPU.
8. **Credentials: bearer tokens work; HMAC and SigV4 don't yet.**
   - **Bearer tokens.** The host injected the GitHub token and the guest
     never saw it, but that holds only by construction. The spike's host
     does not:
     - strip `Cookie`, `Proxy-*` or `X-Amz-*` headers;
     - bind the secret to one host;
     - re-check the allowlist on redirects (N1).
   - **Webhooks.** `Handle` reads the webhook secret through `Getenv`,
     which the guest doesn't have. Under C-INGEST-2 and C-INGEST-9,
     verification belongs to the host, before any parsing. It should not
     be an adapter capability.
   - **AWS.** The SDK works over the `RoundTripper` both unsigned and
     SigV4-signed in the guest. The in-guest signing only proves
     feasibility: it puts AWS keys in the guest and violates C-ADAPTER-6.
     Host-side SigV4 must strip any guest `Authorization`/`X-Amz-*`
     headers and compute the payload hash itself.
9. **The AWS SDK is feasible with upstream Go.** STS alone is a 13.5 MiB
   module (3.4 MiB gzip). ADR 9's remote-runtime fallback isn't needed for
   AWS on technical grounds.
10. **Validation stays on the host.** The harness validated every
    observation host-side with `model.Validate`; the guest needs no
    validator. Linking protovalidate and cel-go into the guest adds
    13.8 MiB to the upstream Go module and 4.7 MiB to the TinyGo module,
    and the TinyGo build crashes.
11. **C-ADAPTER-1 can't be met with Go-built modules.** Every module
    imports WASI preview1, so a "no WASI" host refuses to load them.
    - **Upstream Go** imports 21 functions: `args_*`, `environ_*`,
      `clock_time_get`, `random_get`, `poll_oneoff`, `sched_yield`,
      `proc_exit`, `fd_*` and `path_open`/`path_filestat_get`/`path_readlink`.
    - **TinyGo** imports 15, including `sock_send`/`sock_recv`.
    - **The harness** enabled WASI with real wall and monotonic clocks,
      wazero's default random source, which is deterministic (seed 42), so
      `crypto/rand` and map hash seeds are predictable, and the host's
      stderr.

    C-ADAPTER-1 has to be rewritten as a WASI profile (see the amendment
    list).
12. **The host and embedded modules cost size per adapter.** The host
    costs +5.2 MiB once. Each embedded first-party module costs its own
    size, because `go:embed` doesn't compress. See "Release packaging".

## Release packaging (C-ADAPTER-9 / C-SUPPLY-6)

The coordinator's decision is to keep first-party modules embedded in the
binary, built from this repository in the same build and never downloaded.
On that basis, the per-adapter cost is:

| Per first-party adapter | Embedded raw | Embedded gzip (decompressed on load, sha256 checked against the build) |
| --- | ---: | ---: |
| upstream Go, lean | 12.5 MiB | 3.3 MiB |
| upstream Go, lean + protobuf-go (finding 3) | ~16 MiB | ~4.5 MiB (estimate) |
| upstream Go, AWS SDK (STS only; more services add more) | 13.5 MiB+ | 3.4 MiB+ |
| TinyGo, lean (where it builds) | 2.0 MiB | 0.7 MiB |

The binary is today's 22.9 MiB, plus 5.2 MiB for the host, plus the
modules. For the planned three first-party adapters (GitHub, AWS,
PagerDuty), built with upstream Go:

- **Embedded raw:** ~66–76 MiB.
- **Embedded gzip:** ~38–42 MiB.

Each adapter also costs one cold compile, 7–14 s, the first time a binary
version starts. After that, the compilation cache serves it.

**Is a package without bundled adapters justified now?** Not yet, if
modules are embedded compressed. Three Go adapters add ~13 MiB over the
host, a ~45% increase for the full binary. That is within what one binary
carries. It becomes justified at either point:

- The adapter set grows past roughly six Go adapters (≥ 25 MiB of
  compressed modules).
- Distributed installs (ADR 7) run non-adapter roles, such as API, query
  and ingest, that never execute adapters.

**Recommendation.** Ship one `bearing` binary with first-party modules
embedded gzip-compressed. Keep a `noadapters` build tag from the start, so
a `bearing-core` package costs one CI job when one of those triggers hits.
Add a CI size budget per embedded module. External modules still load only
by sha256 digest (C-ADAPTER-9).

## Security properties verified / not verified

Verified in the spike:

- Requests to a host or method outside the grant were refused with
  "permission denied" in every variant (host mismatch only).
- The guest never received the GitHub token; the host injected it.
- Observations were validated on the host. The guest linked no validator.
- A 128 MiB `MaxPages` cap and a smaller 10 MiB cap were enforced: the
  guest trapped and the host got an error.
- `CloseOnContextDone` plus a deadline stopped a running sync within tens
  of ms of the deadline.
- Modules import only Extism's kernel or `bearing`, `bearing_call` and WASI
  preview1. The import lists are recorded in finding 11.

Not verified:

- A hostile guest. The spike didn't test a module that calls
  `environ_get`/`path_open`, names unknown capabilities or ungranted
  methods, sets its own `Authorization`, follows a cross-host redirect,
  spins in a CPU loop with no host calls, or floods logs or output. The
  deadline test interrupted guest code, but it wasn't a pure loop.
- The `http` capability's SSRF defenses (C-ADAPTER-5) aren't implemented.
  The spike's host uses `http.Client` defaults, which:
  - follow redirects without re-checking the allowlist;
  - honor `HTTPS_PROXY`;
  - allow `http://`;
  - do no resolved-IP checks.
- Header stripping and per-host secret binding (C-ADAPTER-6).
- Size caps on capability request and response fields. The host compares
  hostnames without canonicalizing them. It silently truncates responses
  over 16 MiB instead of returning an error. The raw ABI ignores
  `mem.Read`'s `ok` result.
- The `clock` capability silently falls back to the WASI clock when it is
  denied. SDK clients should surface permission errors instead.
- A WASI profile: empty args and env, no preopens, a cryptographic random
  source, a clock decision, and bounded, rate-limited output. Also,
  `EXTISM_ENABLE_WASI_OUTPUT` must be refused (N6).
- Compilation-cache integrity: permissions, ownership and keying.
- Host-side SigV4 and host-side webhook verification.

## ADR 9 amendment should include

1. **A rewritten C-ADAPTER-1: a WASI preview1 profile.**
   - Link preview1 with empty `args` and `env`, no filesystem preopens, no
     sockets or listeners.
   - Discard stdout/stderr, or bound and rate-limit them; the host owns
     the output writers.
   - Use `WithRandSource(crypto/rand.Reader)`.
   - Decide on clocks. Proposal: a fake wall clock, so real time comes
     only through the `clock` capability, and a real monotonic clock for
     the Go runtime. Verify Go's runtime and GC under that choice.
   - Refuse to start if `EXTISM_ENABLE_WASI_OUTPUT` is set.
   - Any import outside the profile fails the load.
2. **Concrete C-ADAPTER-3 limits.**
   - Every call runs under a deadline with `CloseOnContextDone`, and a
     `MaxPages` cap of 64–128 MiB. Budget for the ~2× CPU cost measured
     above.
   - Response and observation size caps.
   - The instance is discarded on any breach.
3. **`http` capability rules (C-ADAPTER-4/5/6).**
   - HTTPS only; exact hostnames; GET/HEAD by default.
   - A dialer that checks the resolved IP and dials the address it
     checked.
   - `CheckRedirect` re-checks every hop, at most 5.
   - `Proxy: nil`.
   - Strip guest-set `Authorization`, `Cookie`, `Proxy-*`, `Host` and
     `X-Amz-*`; bind each secret to one host.
   - Field size caps; an oversize response is an error, not a truncation.
4. **Credential modes are host-side only.**
   - Bearer-token injection.
   - SigV4 signing in the host, which computes the payload hash itself.
   - Webhook verification in the host before parsing (C-INGEST-2,
     C-INGEST-9). There is no `hmac.verify` capability.
5. **Provenance.**
   - First-party modules are embedded, compressed, with the digest
     recorded at build time and checked before compile (C-SUPPLY-6).
   - External modules load by sha256 digest, with an optional signature
     check before compile (C-ADAPTER-9).
6. **Compile-cache control: a new T-ADAPTER-12 / C-ADAPTER-12.**
   - The cache holds native code.
   - It is local only, never shared or on the network.
   - Its directory is owned by the Bearing user with mode 0700, checked at
     start-up.
   - It is keyed by module digest and wazero version.
7. **A hostile-guest conformance module.** It tries `environ_get`,
   `path_open`, an unknown capability, an ungranted method, its own
   `Authorization`, a cross-host redirect, an infinite loop, memory growth
   and an output flood. Every runtime and capability provider must pass it.
8. **ABI choice weighed on attack surface as well as speed.** Extism adds
   kernel imports, dependencies and glob host matching. A thin wazero ABI
   exposes only `bearing_call` plus the WASI profile.

## Recommendation

**Go**: accept ADR 9 with WASM as the default adapter runtime, amended as
listed above. Ship one `bearing` binary with the host and embedded,
compressed first-party modules, as described in "Release packaging". The
conditions:

1. Split `pkg/telemetry` into an API part and a setup part. Adapters and
   `pkg/adapter` import only the API part, or the protocol types move to
   their own package. Add a CI size budget per module.
2. Use upstream Go (`wasip1`, `go:wasmexport`) as the supported Go
   toolchain. Treat TinyGo as an optional size optimization that each
   adapter must prove with the conformance suite.
3. Compile modules when a Source is applied, or at first start for
   embedded modules. Keep a persistent compilation cache under C-ADAPTER-12
   rules. Never compile on the sync path.
4. Decide the ABI on copy cost and attack surface: get caller-memory or
   bulk-copy access into Extism upstream, or keep `bearing_call` on a thin
   wazero ABI. Keep `bearing_call`'s shape either way.
5. Messages are Protobuf with `bytes` bodies. Measure protobuf-go's guest
   size (+3.6 MiB) and CPU first.
6. Credentials never enter the guest: host-side SigV4 and host-side webhook
   verification, as in amendment item 4.
7. Validation stays on the host. Guests link no protovalidate or cel-go.
8. Implement the `http` capability rules and the WASI profile (amendment
   items 1 and 3). The hostile-guest module must pass before any adapter
   is enabled by default.
9. Every guest call runs under a deadline with `CloseOnContextDone` and a
   64–128 MiB `MaxPages` cap. Size worker pools for the measured cost:
   ~2× guest CPU with interruption on, and 1.3–1.6 s wall time per sync of
   this size, most of it guest CPU. Re-measure under load with realistic API latency.
