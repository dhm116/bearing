# Threat model

Status: draft for the MVP · Last reviewed: 2026-10-10

This document describes Bearing as planned for the MVP, where it trusts what,
what can go wrong at each trust boundary, and the control that answers each
threat. It follows ADRs 2 and 4–12 plus the MVP security decisions. ADR 9,
accepted with conditions after the
[WASM adapters spike](https://github.com/dhm116/bearing/blob/spike/wasm-adapters/docs/spikes/wasm-adapters.md), makes WASM modules the
default adapter runtime once [M4](https://github.com/dhm116/bearing/milestone/5) lands.
Adapters that can't run as WASM run as local processes serving the
Protobuf adapter service on a Unix socket, with the exceptions in B2; ADR
3's stdio transport is retired (ADR 9 A13). Where
an ADR says otherwise, this document records the decision and the ADR is
to be amended. ADRs 7, 8 and 9 now carry the main amendments: the host,
not the adapter, authenticates webhook deliveries (C-INGEST-2, ADR 9 A6),
audit checkpoints live outside the store (C-AUDIT-3), and Extism's own
host functions are off (C-ADAPTER-1). Where today's code differs, the
control is a requirement on the code.

To report a vulnerability, see [SECURITY.md](../../SECURITY.md).

## How to use it

- Every control has a stable ID, `C-<AREA>-<n>`. Milestone criteria, tests
  and reviews cite these IDs. IDs are never renumbered or reused. A control
  that is dropped stays listed, marked **Retired**, with the reason.
- Threats have IDs too (`T-<AREA>-<n>`) and use STRIDE letters: **S**poofing,
  **T**ampering, **R**epudiation, **I**nformation disclosure, **D**enial of
  service, **E**levation of privilege.
- A change that adds a trust boundary, an input or a setting that weakens a
  control updates this document in the same pull request.

## The system

```
 [B3] source systems ─push─▶ [B1] ingest ─▶ event log ─▶ workers ─▶ [B6] store
      (GitHub, …)                                        │  ▲      (PostgreSQL: graph,
           ▲                                             ▼  │       vectors, log, config,
           └──── http capability ◀── [B2] adapter (WASM) ◀┘  │       audit, kv)
                                                            │
 people, CLI ── Unix socket / TCP+OIDC ──▶ [B4] API ────────┤
 AI agents ─────────── OIDC ─────────────▶ [B4] MCP ────────┘

 [B5] identity provider (OIDC, JWKS)     [B7] operators and config
 [B8] build and release supply chain     [B9] model providers (embeddings)
```

- **`bearing server`** runs ingest, the scheduler, workers, the API and the
  MCP server in one process for the MVP.
- **Ingest** receives pushed events (HTTP webhooks, CloudEvents) and appends
  them to the event log (ADR 7).
- **Workers** run adapters as WASM modules (ADR 9). Adapters reach source
  systems only through host capabilities and emit observations. Workers apply
  the result to the graph in one transaction with its audit record (ADR 8).
- **The store** is PostgreSQL with pgvector: graph, vectors, event log,
  config, audit log and adapter `kv` (ADR 14).
- **The API** serves queries and configuration (ADR 10). **MCP** serves
  read-only queries to AI agents.
- **The identity provider** (Authentik or any OIDC provider) authenticates
  remote callers. Bearing never mints, issues or stores credentials.

## Assets

| ID | Asset | Why it matters |
| --- | --- | --- |
| A1 | Graph facts: ownership, on-call, dependencies | Wrong facts page the wrong team or, later, grant the wrong access |
| A2 | Source credentials (API tokens, webhook secrets) | Read access to the organization's tools |
| A3 | Organization data: repos, people, teams, incidents | Confidential; names people |
| A4 | Audit log | The record of who changed what and why |
| A5 | Configuration: sources, grants, role mappings | Controls what adapters can reach and who can do what |
| A6 | Store credentials | Full access to A1, A3, A4, A5 |
| A7 | Availability of ingest, API and MCP | Stale answers are wrong answers |

## Controls that apply everywhere

### Secure defaults, validation and logging

- **C-GEN-1** Every setting that weakens a control is named `insecure_*`.
  Each one in effect is logged at WARN on start and whenever config that
  enables it is applied, the change is audited, and `bearing status` lists
  it. No other setting may weaken a control.
- **C-GEN-2** Defaults are the secure choice: ingest off, TCP API off, hosted
  model providers off, adapters granted nothing they did not declare,
  unmapped callers denied.
- **C-GEN-3** Data entering Bearing (pushed events, adapter output, config,
  API requests) is checked against its Protobuf type and the validation
  rules at the edge, before use (ADR 6). The rules are Go functions in
  `pkg/model` (`ValidateObservation`, `ValidateDeclaration`,
  `ValidateManualEvent`), not protovalidate annotations.
- **C-GEN-4** Logs are structured. Source text appears only in attribute
  values, never in the message or in attribute keys, with control characters
  escaped.

### Secrets

- **C-SECRET-1** Secrets are configured only as references: `env:NAME` or
  `file:/path` in the MVP. A config field that takes a secret rejects
  anything that is not a reference. For Source and Adapter resources,
  `env:` names must match a configured allowlist (default
  `BEARING_SECRET_*`) and `file:` paths must be inside a configured secrets
  directory (default `/run/secrets/bearing/`). `file:` paths are checked
  after resolving symlinks and `..`. Bearing's own settings (store password,
  OIDC) cannot be referenced by a Source or Adapter resource.
- **C-SECRET-2** Resolved secret values are never stored, logged, traced,
  put in errors, returned by the API or MCP, or written to audit records.
- **C-SECRET-3** Adapter code never sees secret values. The host injects
  them into outbound requests (C-ADAPTER-6) and webhook checks (C-INGEST-2).
  Exception: a local-process adapter receives the values of the secrets
  its own Source names, and no others, delivered by the core as in
  C-ADAPTER-20, never through its environment. This limits what a correct
  adapter receives; it does not contain a malicious one (see
  "Local-process adapters" in B2; ADR 9 A13, A15).
- **C-SECRET-4** Resolved values are registered with the logger and tracer,
  which replace any occurrence with `[redacted]`. This is a backstop, not the
  primary control; tests assert that a known secret never appears in log or
  span output.
- **C-SECRET-5** A `file:` secret that is group- or world-readable produces a
  start-up warning.

### Audit log

- **C-AUDIT-1** Every fact status change, config change, role decision on an
  admin operation and confirmation by a person is written to the audit log
  in the same transaction as the change (ADR 8). No change commits without
  its record. Individual support writes are in the change journal, which is
  not hash-chained. Until the audit log (#138) exists, a `ChangeSet`'s audit
  entries are kept only in the change journal, which holds the whole
  `ChangeSet`; C-AUDIT-1 is met for that copy and the entries are not yet
  readable through any contract. Their text (reason, rule, IDs) is untrusted
  input of bounded size: whatever prints it escapes control characters.
- **C-AUDIT-2** Each record carries the SHA-256 hash of the previous record
  over a canonical encoding, forming a chain.
- **C-AUDIT-3** At an interval, Bearing writes a checkpoint (sequence number,
  head hash and time) outside the store: to the log stream, a file or the
  configured exporter. When a signing key is configured (a secret
  reference, C-SECRET-1), the checkpoint is signed. A rewrite of the whole
  chain in the store no longer matches the checkpoints.
- **C-AUDIT-4** `bearing audit verify` checks the chain and every checkpoint
  it is given, including signatures, and reports the first record that
  fails.
- **C-AUDIT-5** The `AuditLog` contract has no update or delete. Retention
  removes only the oldest records, after writing a checkpoint at the cut.
- **C-AUDIT-6** Records name actors by the stable ID the authenticator gives
  (the OIDC subject, client ID, `local:<uid>`) or, for a component,
  `system:<name>`, which only the core sets; the API rejects an
  authenticated subject with that prefix, or with `local:` unless it comes
  from the local socket. Records never contain tokens or
  secret values. Whether a person's ID also carries its issuer is decided
  with authentication (ADR 12).

## Trust boundaries

### B1. Ingest: pushed events and their senders

Anyone who can reach an ingest port can send bytes. Only a configured Source
holding the right secret, certificate or token is trusted, and only for its
own events.

Assets: A1, A7, the event log.

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-INGEST-1 | S | Attacker posts a forged event that claims to come from GitHub | C-INGEST-2, C-INGEST-3 |
| T-INGEST-2 | S | A valid credential for Source A is used to inject events for Source B | C-INGEST-3 |
| T-INGEST-3 | T | Payload altered in transit | C-INGEST-2, C-INGEST-6 |
| T-INGEST-4 | T, R | A captured valid delivery is replayed, or an old event overwrites newer facts | C-INGEST-5 |
| T-INGEST-5 | D | Large bodies, slow clients or floods exhaust memory, the log or the store | C-INGEST-4, C-INGEST-8 |
| T-INGEST-6 | E | Parser bug reached by unauthenticated input | C-INGEST-1, C-INGEST-9, C-GEN-3 |
| T-INGEST-7 | I | Error responses reveal which sources exist or echo input | C-INGEST-7 |
| T-INGEST-8 | D | An event is acknowledged but lost, so the sender never retries | C-INGEST-8 |
| T-INGEST-9 | S | A forged `X-Forwarded-For` evades per-peer limits or poisons logs | C-INGEST-10 |

- **C-INGEST-1** Ingest is a separate surface from the API, made of pluggable
  transports (HTTP webhooks and CloudEvents in the MVP). Each transport is
  configured on its own (address, port, protocol, TLS) and can be turned off.
  Ingest is off until a Source that pushes events is configured.
- **C-INGEST-2** The host authenticates every pushed event for its Source
  before anything is stored or acknowledged, using the method the Source
  configures: an HMAC signature over the raw body; a client certificate
  chaining to the Source's CA bundle, with its SAN or SHA-256 fingerprint
  pinned in the Source; a static token by reference; or an OIDC access token
  whose client ID or group is mapped to `ingest` for this Source (C-API-4).
  Comparisons are constant
  time. A Source with no method configured cannot receive events. Failures
  return 401, store nothing and increment a metric. WASM modules never
  verify deliveries or see webhook secrets: `Handle` receives only
  deliveries the host has authenticated, and there is no HMAC capability
  (ADR 9 A6). A local-process adapter may also verify, as defense in
  depth, but the host authenticates first. Ingest stays off (C-INGEST-1)
  until the host verifier exists; it ships with the first ingest
  transport. Until then an adapter's `Handle` is reached only through
  local or test paths,
  and the adapter's own verification is the check (ADR 9 A15).
- **C-INGEST-3** Each request is routed to exactly one Source by its
  configured route and checked with that Source's credential. The Source
  recorded on the event comes from the route, never from the payload.
- **C-INGEST-4** Limits per transport: maximum body size (default 1 MiB,
  enforced while reading), header size, read and idle timeouts, and maximum
  concurrent requests.
- **C-INGEST-5** Duplicates are dropped by a key scoped to the Source (ADR
  7): the delivery ID when the signature covers it, otherwise the SHA-256 of
  the authenticated body. Where the sender signs a timestamp, events outside
  a configured window (default 5 minutes) are rejected. Webhook payloads are
  change hints. Facts carry the source's own update time, and an older
  observation never replaces a newer assertion. Where the source gives no
  update time, event-log order decides.
- **C-INGEST-6** Transports bind TLS 1.2 or later. Plain HTTP on a
  non-loopback address requires `insecure_ingest_plaintext` (for example
  behind a TLS-terminating proxy).
- **C-INGEST-7** Ingest responses carry only a status code and a request ID.
  An unknown route and a failed check get the same response.
- **C-INGEST-8** Per-Source and per-peer rate limits return 429. The 2xx is
  sent only after the event is appended to the log (ADR 7); if the log is
  unavailable, ingest returns 503 so the sender retries.
- **C-INGEST-9** The body is parsed only after authentication succeeds, into
  a fixed Protobuf or JSON type, with depth and size limits.
- **C-INGEST-10** Forwarded headers are ignored unless the peer is in
  `trusted_proxies`.

### B2. Adapter modules

An adapter module is code Bearing did not write and must not trust, even
first-party ones, because they parse attacker-influenced data. The module
runs inside the host process, so the sandbox is the boundary.

Assets: A2, A3, A1 (through emitted observations), the host process, the
internal network, cloud metadata endpoints and the compilation cache.

**Local-process adapters** (ADR 9 A13) are adapters that can't run as
WASM. The core starts each one as a child process serving the Protobuf
adapter service (Connect/gRPC) on a Unix domain socket, the same service a
remote adapter serves; prior art is HashiCorp go-plugin. They replace ADR
3's stdio JSON-RPC transport, which is retired.

They are not sandboxed. A local-process adapter running as the service
user is equivalent to the host: it can read whatever the service user can,
including secrets files, the store password and the compilation cache;
it can reach the store and the network directly; it can use the local API
socket as `admin` (C-API-1); and it can connect to other local adapters'
sockets. C-ADAPTER-2, 7, 11 and
16 and C-SECRET-3's "no others" limit what a correct adapter receives and
what the core accepts from it; they do not contain a malicious one.
Operators run only local-process adapters they would run as the service
user. Running each local-process adapter under its own unprivileged UID is
a planned hardening option: C-ADAPTER-13 and 14 are written against the
UID the core started the child as, so the core's start contract allows it
without changing the SDK.

What applies to them:

- Waived: the sandbox controls C-ADAPTER-1, 4, 5, 6, 8 and 12, and
  C-ADAPTER-3's in-guest wall-time and memory limits. C-ADAPTER-17 puts
  limits on the core's side in their place.
- C-GEN-3, C-ADAPTER-7 and C-ADAPTER-11 apply to what the adapter returns.
  C-ADAPTER-2 applies to the declared kinds, relations and key prefixes of
  its grant, enforced on output.
- C-ADAPTER-10 applies through C-ADAPTER-18: stdout is no longer reserved
  for the protocol, so the child logs through telemetry that the core
  captures, tags and forwards.
- C-ADAPTER-13 to 22 cover the socket, the child's environment, secrets,
  lifecycle, platforms and the adapter binary itself.

Today the scaffold's stdio adapter is the only process adapter: it
inherits the server's environment, including `BEARING_STORE_PASSWORD` and
OIDC settings, and its stderr passes through untagged
([#25](https://github.com/dhm116/bearing/issues/25)).

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-ADAPTER-1 | E | Module reads files or environment, opens sockets, or escapes into the host | C-ADAPTER-1 |
| T-ADAPTER-2 | E | Module calls a capability or host it did not declare | C-ADAPTER-2, C-ADAPTER-4 |
| T-ADAPTER-3 | I, E | SSRF to `169.254.169.254`, localhost, the store or internal services | C-ADAPTER-5 |
| T-ADAPTER-4 | I | Module reads or forwards a source token | C-SECRET-3, C-ADAPTER-6 |
| T-ADAPTER-5 | I | Module sends organization data to an attacker's host | C-ADAPTER-4, C-ADAPTER-5 |
| T-ADAPTER-6 | T | Module writes to the source system | C-ADAPTER-4 |
| T-ADAPTER-7 | D | Infinite loop, memory growth, huge output or log flood | C-ADAPTER-3 |
| T-ADAPTER-8 | T | Module emits facts about another system's entities, overrides another Source's facts, or resolves identities | C-ADAPTER-7, C-ADAPTER-11, C-GEN-3 |
| T-ADAPTER-9 | I, T | Module reads or poisons another Source's `kv` cache | C-ADAPTER-8 |
| T-ADAPTER-10 | T | Module file swapped on disk or in a registry | C-ADAPTER-9, C-ADAPTER-22, C-SUPPLY-6 |
| T-ADAPTER-11 | R | Module logs pretend to come from the host | C-ADAPTER-10, C-ADAPTER-18 |
| T-ADAPTER-12 | T, E | Native code in the compilation cache is replaced or shared, or code compiled without termination checks is reused with them required, so a module runs outside its limits | C-ADAPTER-12 |
| T-ADAPTER-13 | S, I | A process running as another UID connects to a local adapter's socket, poses as the adapter or the core, or reads its traffic | C-ADAPTER-13, C-ADAPTER-14 |
| T-ADAPTER-14 | S, D | A stale or pre-created socket path or directory is reused, or cleanup follows a symlink, so the core talks to the wrong process, deletes the wrong file or cannot start the adapter | C-ADAPTER-13, C-ADAPTER-15 |
| T-ADAPTER-15 | I, E | A local adapter inherits the server's environment, including the store password, OIDC settings, exporter credentials or other Sources' secret references | C-ADAPTER-16, C-ADAPTER-18, C-ADAPTER-20 |
| T-ADAPTER-16 | D | A local adapter hangs, never starts, returns huge responses or floods its output | C-ADAPTER-17, C-ADAPTER-18, C-ADAPTER-19 |
| T-ADAPTER-17 | E, D | A local adapter outlives the core, leaves orphaned processes, or its PID is reused before the peer check | C-ADAPTER-14, C-ADAPTER-19 |

T-ADAPTER-13 covers other UIDs only. A process running as the same UID as
the child is equivalent to the host and is an accepted risk; a
per-instance token or mTLS on the socket would add nothing against it,
because that process could read the token or key too.

- **C-ADAPTER-1** Modules run on wazero with a fixed WASI preview1 profile
  (ADR 9 A8), because every Go-built module imports preview1:
  - Empty args and environment, no filesystem preopens, no sockets or
    listeners. TinyGo's `sock_send`/`sock_recv` are stubs that return an
    error.
  - The host owns stdout and stderr: output is discarded, or bounded and
    rate-limited. It is never the host's own stdout or stderr.
  - The random source is `crypto/rand`. The wall clock is fake, so real time
    comes only through the `clock` capability; the monotonic clock is real.
    Go's runtime and GC are verified under the fake wall clock by the
    hostile-guest module (ADR 9 A12) before this profile is final.
  - The only other host functions are `bearing_call` and its ABI's read
    helper (or, if Extism is chosen, Extism's memory kernel with its HTTP,
    config, var and path functions off: `allowed_hosts` and
    `allowed_paths` empty). Bearing refuses to start if
    `EXTISM_ENABLE_WASI_OUTPUT` is set.
  - A module importing anything outside the profile fails to load.
- **C-ADAPTER-2** The effective grant is the manifest's declaration narrowed
  by the Source; a Source can narrow but never widen it. Config apply rejects
  a widening. A call outside the grant returns a permission error, is logged
  and is counted. `bearing diff` shows the effective grant before apply.
- **C-ADAPTER-3** Per-call limits (ADR 9 A9):
  - Wall time: every guest call runs under a deadline with wazero's
    `CloseOnContextDone`, so a pure CPU loop is stopped too.
  - Memory: a cap of 64–128 MiB per instance, counted across all of its
    memories (an Extism instance has two).
  - Size caps on every capability request and response field, on the
    number and size of observations, on `kv` keys and bytes, and on log
    records and guest output. An oversize response is an error, never a
    truncation.

  A call that exceeds a limit is aborted and the module instance
  discarded. The hostile-guest conformance module (ADR 9 A12) exercises each
  limit, and every runtime must pass it before any adapter is enabled by
  default.
- **C-ADAPTER-4** The `http` capability allows only HTTPS to hosts on the
  effective allowlist, and only GET and HEAD unless the manifest declares
  other methods and the Source grants them. `http://` URLs are refused,
  including as redirect targets. Only port 443 is allowed unless the grant
  names a port. Hostnames are canonicalized (lowercase, IDNA to punycode,
  no trailing dot) and must match an allowlist entry exactly; no globs.
- **C-ADAPTER-5** After DNS resolution the host refuses loopback, private
  (RFC 1918, `fc00::/7`), link-local (`169.254.0.0/16` including
  `169.254.169.254`, `fe80::/10`), CGNAT (`100.64.0.0/10`), `0.0.0.0/8`,
  `198.18.0.0/15`, `64:ff9b::/96`, `2002::/16`, `fec0::/10`, `::/128`,
  multicast and broadcast addresses, including IPv4-mapped IPv6 forms. The
  check runs in the dialer on every resolved address, and the dialer dials
  the address it checked, so DNS rebinding cannot swap it. `CheckRedirect`
  re-checks every hop's scheme, host and resolved address against
  C-ADAPTER-4 and these rules; at most 5 redirects. The capability's
  transport sets `Proxy: nil`, so `HTTPS_PROXY`, `HTTP_PROXY` and
  `NO_PROXY` are ignored; a shared egress proxy is a capability provider
  (ADR 9), not an environment setting. A Source may allow named private
  CIDRs (for example a GitHub Enterprise server) only with
  `insecure_allow_private_networks`.
- **C-ADAPTER-6** The host injects credentials only for the host the secret
  is bound to, and drops them on a cross-host redirect. It strips
  `Authorization`, `Cookie`, `Proxy-*`, `Host` and `X-Amz-*` headers set by
  the module. Credential modes are host-side only: bearer injection and AWS
  SigV4, where the host computes the payload hash itself (ADR 9 A6).
- **C-ADAPTER-7** Adapter output is validated (C-GEN-3). Keys must use the
  system prefixes in the effective grant, and kinds and relations must be in
  the manifest's declared set. Only the core links keys across systems.
- **C-ADAPTER-8** `kv` is scoped to one Source by the host; a module names
  keys only inside its own scope. Values read back are treated as untrusted.
- **C-ADAPTER-9** First-party adapters are built into the binary,
  gzip-compressed, with their sha256 recorded at build time and checked
  before compile. External modules load only by sha256 digest from config;
  a mismatch refuses the load.
- **C-ADAPTER-10** Adapter logs and spans are tagged with adapter and Source,
  their attributes are prefixed `adapter.`, and they are rate-limited.
- **C-ADAPTER-11** Every fact assertion records the Source that made it.
  Applying a Source's observations can create, change or retract only that
  Source's assertions, never another Source's. The key prefixes a module may
  use are part of its effective grant and appear in `bearing diff`.
- **C-ADAPTER-12** Modules are compiled when a Source is applied, or at
  first start for embedded modules, never on the sync path. The compilation
  cache holds native code and is protected:
  - It is local only, never shared between hosts or on a network
    filesystem.
  - Its directory is owned by the Bearing user with mode 0700; start-up
    checks this and refuses to use a cache that fails.
  - Entries are keyed by wazero's own cache key (module bytes,
    listener and termination-check flags, CPU features) plus the wazero
    version. Bearing never substitutes a bare module digest, so code
    compiled without termination checks is never reused with
    `CloseOnContextDone` on.
  - The hostile-guest CPU-loop case also runs against a warm cache.
- **C-ADAPTER-13** The socket lives in a private directory per instance:
  - The parent runtime directory is `$XDG_RUNTIME_DIR/bearing`, systemd's
    `RuntimeDirectory`, or a configured path. Start-up checks it with
    `Lstat`, as C-ADAPTER-12 does for the cache: not a symlink, owned by
    the UID the core runs as, mode 0700. The core refuses one that fails.
  - For each local-process adapter instance, the core creates a new
    directory inside it with `MkdirTemp`, mode 0700, and holds an `flock`
    on a lock file in it while the instance runs. By default the child
    runs as the service user, so the directory is that user's alone; a
    child started under its own UID gets access for that UID only.
  - Under a separate child UID, the parent runtime directory allows
    traversal for that UID only (mode 0711 or an ACL). Either the core
    sets the socket file's owner and mode after the child binds it, or the
    core binds the socket itself and passes the listening descriptor to
    the child.
  - The socket is an absolute filesystem path in that directory, never an
    abstract (`@`) socket, and stays within the OS limit for socket paths
    (104 bytes on macOS, 108 on Linux).
  - The core passes the path to the child in one place, the
    `BEARING_ADAPTER_SOCKET` environment variable. The child serves the
    adapter service there and nowhere else.
  - The process the core starts must listen on the socket itself; a
    wrapper script `exec`s the adapter rather than forking it. Otherwise
    the peer check (C-ADAPTER-14) fails, and the core's error names the
    expected and actual PID.
- **C-ADAPTER-14** On every connection the core checks the peer where the
  OS supports it. The UID must be the one the core started the child as.
  The PID, where the OS reports it (`SO_PEERCRED` on Linux, `LOCAL_PEERPID`
  on macOS), must be the child's, and the core checks it while the child
  is still unreaped, so the PID cannot have been reused. Where available,
  the core compares process file descriptors instead of PIDs
  (`SO_PEERPIDFD` against the child's pidfd on Linux). `getpeereid` on the
  BSDs gives the UID only. The same check applies to connections the core
  accepts on the OTLP receiver socket (C-ADAPTER-18). A failed check
  closes the connection, stops the child, fails the call and is logged
  and counted.
- **C-ADAPTER-15** When an adapter stops or crashes, the core closes its
  connections and removes the socket and its directory. At start-up the
  core removes directories an earlier run left, only inside the verified
  parent runtime directory (C-ADAPTER-13), without following symlinks, and
  skipping any directory whose lock file a live process still holds.
- **C-ADAPTER-16** A local-process adapter starts with a minimal
  environment: `BEARING_ADAPTER_SOCKET`, the adapter and Source it serves
  (for tagging), and the minimum the OS needs (for example `PATH`, `HOME`,
  `TMPDIR`). It never receives secret values or secret references, OIDC
  settings, `BEARING_STORE_PASSWORD`, telemetry exporter endpoints or
  headers, or other Bearing settings. This is a requirement on the code,
  and it lands before any always-on server (M3) runs local-process
  adapters ([#25](https://github.com/dhm116/bearing/issues/25)).
- **C-ADAPTER-17** The core limits a local-process adapter from its own
  side, in place of the in-guest limits of C-ADAPTER-3:
  - Every RPC runs under a deadline, and start-up has its own deadline:
    a child that does not serve the socket and answer `Describe` in time
    is killed.
  - Responses are read with a size cap (the client's maximum receive
    message size), and each `Sync` page has caps on the number and size
    of observations.
  - stdout, stderr and telemetry from the child are bounded and
    rate-limited (C-ADAPTER-18). Overflow is dropped and counted, not
    treated as a breach.

  A breach of the RPC deadline, the start-up deadline or a size cap fails
  the call and kills the child, which is restarted under C-ADAPTER-19's
  backoff.
- **C-ADAPTER-18** A local-process adapter's logs and telemetry go to the
  core, never straight to an exporter. The core captures its stdout and
  stderr, and may serve an OTLP receiver on a second socket in the same
  private directory. The core tags what it receives with adapter and
  Source, prefixes attributes with `adapter.`, rate-limits it and forwards
  it to its own telemetry (C-ADAPTER-10). The child never receives
  exporter endpoints or headers.
- **C-ADAPTER-19** The core owns the child's lifecycle:
  - The child runs in its own process group, and stopping it kills the
    group.
  - On Linux, the child is started with `Pdeathsig` set to `SIGKILL`, so
    it dies with the core. `Pdeathsig` fires when the starting thread
    exits, so the goroutine that starts the child stays locked to its OS
    thread for the child's whole life.
  - Everywhere, the child holds the read end of a pipe from the core and
    exits on EOF, so it exits when the core is gone.
  - A start-up handshake deadline applies (C-ADAPTER-17), and a child that
    keeps failing is restarted with exponential backoff.
  - The peer check (C-ADAPTER-14) runs before the child is reaped, using
    pidfds where available.
  - Descendants left behind when the core crashes are cleaned up by the
    service manager (systemd `KillMode=control-group`, or the container
    exiting). On Linux the core may also set `PR_SET_CHILD_SUBREAPER` and
    kill leftover process groups at start-up.
- **C-ADAPTER-20** The core resolves the secrets the adapter's Source names
  (C-SECRET-1) and sends their values to the child once, over an inherited
  pipe file descriptor or a `Configure` RPC on the checked socket. Values
  never go in the environment, arguments or files, and the child never
  receives secret references, so it cannot resolve any other secret
  through Bearing. C-SECRET-2 and C-SECRET-4 still apply on the core's
  side.
- **C-ADAPTER-21** The core refuses to start local-process adapters on
  Windows until a Windows control is specified and tested: an ACL on the
  socket directory in place of mode 0700, and `SIO_AF_UNIX_GETPEERPID` for
  the peer check. `AF_UNIX` exists from Windows 10, but nothing here is
  verified there.
- **C-ADAPTER-22** A local-process adapter is configured by absolute path
  plus sha256; the sha256 field is part of the M4 Source and Adapter
  configuration. Before each start the core checks that the file and every
  directory above it are not writable by other users, opens the file,
  verifies the digest from that descriptor and, on Linux, executes that
  same descriptor (`/proc/self/fd/N` or `execveat`), so the file cannot
  be swapped between check and use. A mismatch refuses the start.

### B3. Data from source systems

Text from source systems is untrusted, even when the system is trusted. PR
titles, commit messages, READMEs, CODEOWNERS and `catalog-info.yaml` are
written by anyone with access to a repository, including attackers and
contributors from outside the organization.

Assets: A1, the people and agents who read Bearing's answers, model
components (`Extractor`, `Judge`).

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-SOURCE-1 | T | A pull request or branch changes ownership before review | C-SOURCE-1 |
| T-SOURCE-2 | T | A repo's `catalog-info.yaml` claims another team's Component or System | C-SOURCE-2 |
| T-SOURCE-3 | T | Two sources disagree and the attacker's claim wins | C-SOURCE-3 |
| T-SOURCE-4 | E | Prompt injection in source text steers an agent reading Bearing | C-SOURCE-4, C-MCP-2 |
| T-SOURCE-5 | T | Prompt injection makes an `Extractor` or `Judge` create false facts | C-SOURCE-5 |
| T-SOURCE-6 | D | Hostile YAML (alias bombs, deep nesting, huge files) | C-SOURCE-6 |
| T-SOURCE-7 | T | Escape sequences, newlines or markup in names reach a terminal, a UI or the logs | C-SOURCE-7, C-GEN-4 |
| T-SOURCE-8 | S | A user in one system is merged with a different person in another by name | C-SOURCE-8 |

- **C-SOURCE-1** Ownership claims (CODEOWNERS, `catalog-info.yaml`) are read
  only from the repository's default branch.
- **C-SOURCE-2** A repository may claim only its own Component. Claims about
  other entities are ignored and counted.
- **C-SOURCE-3** Conflicting ownership claims lower the fact's confidence and
  are flagged. Ownership and policy answers read only asserted facts
  (ADR 2).
- **C-SOURCE-4** Source text is stored and returned as data. Bearing never
  puts it into instructions, tool descriptions or prompts it controls,
  except as a delimited, labelled field.
- **C-SOURCE-5** `Extractor` output is a candidate, not a fact, until a
  `Judge` accepts it; `Judge` returns only typed values. Facts derived from a
  model are marked inferred and never used for ownership or policy answers.
- **C-SOURCE-6** Structured formats are parsed with size and depth limits;
  YAML aliases and custom tags are rejected.
- **C-SOURCE-7** The CLI strips control characters from source text before
  printing to a terminal. Any HTML surface escapes output.
- **C-SOURCE-8** Identity resolution uses stable source IDs and explicit
  alias facts. It never merges on display names or other free text.

### B4. API and MCP clients, including AI agents

Callers are people (CLI, future UI), automation and AI agents. Agents may be
steered by text they read, including text from Bearing itself.

Assets: A3, A5, A4, A7.

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-API-1 | S | Unauthenticated remote access | C-API-2, C-IDP-1 |
| T-API-2 | I, E | Another local user reaches the socket on a shared host | C-API-1 |
| T-API-3 | E | A reader changes config or triggers admin operations | C-API-4, C-API-5 |
| T-API-4 | E | An agent gains more than read | C-API-4, C-MCP-1 |
| T-API-5 | D | Costly queries (deep traversals, large vector searches) | C-API-6 |
| T-API-6 | I | Errors expose queries, stack traces or internal hosts | C-API-7 |
| T-API-7 | R | Nobody can tell who changed config | C-AUDIT-1, C-OPS-2 |
| T-API-8 | I | Credentials or data cross the wire in clear text | C-API-3 |
| T-MCP-1 | E, T | A prompt-injected agent uses Bearing to act | C-MCP-1, C-MCP-2 |
| T-MCP-2 | S | A web page uses DNS rebinding or cross-site requests against a local MCP server | C-MCP-3 |
| T-MCP-3 | I, E | The caller's token is passed on to another service | C-MCP-4 |

- **C-API-1** Local access is a Unix socket with mode 0600 in a directory
  with mode 0700, owned by the service user. The server checks the peer's
  UID: only the service user's UID and root are accepted; others are
  refused and logged. Accepted local callers have `admin` and are recorded
  as `local:<uid>`.
- **C-API-2** The TCP API listener is off by default and refuses to start
  unless OIDC authentication is configured. There is no setting for
  unauthenticated TCP access. Only `/healthz` and `/readyz` are
  unauthenticated, and they return no data. Metrics are served on a separate
  listener, off by default.
- **C-API-3** The TCP listener uses TLS 1.2 or later. Plaintext on a
  non-loopback address requires `insecure_api_plaintext`.
- **C-API-4** Every request is checked by the `Authorizer` contract. The MVP
  backend maps IdP groups and client IDs to roles and denies callers with no
  mapping:
  - `read`: queries and MCP.
  - `ingest`, always granted for named Sources: request syncs
    (`SyncRequested`) of those Sources, and push CloudEvents with an OIDC
    access token to those Sources when they are configured for OIDC
    (C-INGEST-2).
  - `admin`: configuration, audit queries, and `read` and `ingest` for every
    Source.

  A client-credentials token (no user subject) is identified by its
  `azp`/`client_id` and gets only the roles its client ID is mapped to; an
  unmapped client is denied. An **agent** is a client whose only role is
  `read`, for queries and MCP. A client may also be mapped to `ingest` for named Sources
  only. A mapping that gives a client `admin` is rejected at config apply.
- **C-API-5** Each API method declares its required role in one table. A
  test fails if a method has no entry, so new methods are denied until
  classified.
- **C-API-6** Per-caller rate limits, page size limits, query timeouts and a
  cap on traversal depth and vector `k`.
- **C-API-7** Errors return a code, a message written for the caller and a
  request ID. Details go to logs, not responses.
- **C-MCP-1** MCP is read-only. It exposes only query tools that need the
  `read` role: no config, sync, ingest or action tools.
- **C-MCP-2** Source text in MCP results is returned only as objects
  with the fields `untrusted_text`, `source` and `entity_key`, never in tool
  prose.
  Tool names and descriptions are static strings in the binary.
- **C-MCP-3** The HTTP transport checks `Host` and `Origin` against
  configured values and rejects others, and requires OIDC like the API.
- **C-MCP-4** Bearing never forwards a caller's token. It reads sources only
  with its own configured credentials.

### B5. The OIDC identity provider

Bearing trusts the configured issuer to say who a caller is and which groups
they are in. It trusts nothing else in a token.

Assets: caller identity, role mapping, A7 (API availability).

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-IDP-1 | S | Forged token (`alg: none`, HMAC with the public key, unknown key) | C-IDP-1 |
| T-IDP-2 | S | ID token or token for another application accepted | C-IDP-1 |
| T-IDP-3 | S | Token from another issuer, or a spoofed JWKS | C-IDP-1, C-IDP-2 |
| T-IDP-4 | E | Role granted from a claim the user controls (email, name) | C-IDP-3 |
| T-IDP-5 | D | IdP or JWKS endpoint down, slow or oversized, or key-refresh storms | C-IDP-2 |
| T-IDP-6 | I | Bearer tokens leak through URLs, logs, traces or audit records | C-IDP-1, C-IDP-5, C-AUDIT-6 |
| T-IDP-7 | S, E | Bearing becomes a credential store that can be stolen | C-IDP-4 |

- **C-IDP-1** Tokens are verified against the issuer's JWKS: signature,
  exact `iss`, `aud` containing Bearing's configured audience, `exp` and
  `nbf` with at most 60 seconds of skew. Allowed algorithms: RS256, PS256,
  ES256, EdDSA. `none` and HMAC algorithms are rejected. Only access tokens
  are accepted. If the issuer is configured as issuing RFC 9068 tokens,
  `typ` must be `at+jwt`. Otherwise `azp` must be on a configured allowlist,
  and tokens with `nonce` or `at_hash` (ID tokens) are rejected. Tokens are
  read only from the `Authorization` header.
- **C-IDP-2** The JWKS URL comes from the issuer's HTTPS discovery document,
  whose `issuer` must equal the configured issuer exactly. Fetches have
  timeouts and size limits. Keys are cached; an unknown `kid` triggers at
  most one refresh per minute, and keys missing from a refreshed JWKS are
  dropped. With no valid keys, verification fails closed. The Unix socket
  keeps working while the IdP is down.
- **C-IDP-3** Roles come only from the configured groups claim (and, for
  clients, client ID) through the configured mapping. Other claims never
  grant a role.
- **C-IDP-4** Bearing never mints, issues, refreshes or stores credentials.
  There are no local users, passwords or API keys. Agents use the client
  credentials flow at the IdP.
- **C-IDP-5** Tokens are never logged, traced or stored. Only `iss`, `sub`
  and client ID are recorded.

### B6. The store (PostgreSQL)

The store holds everything except secrets. Whoever controls it controls
Bearing's answers. The default backend is PostgreSQL with pgvector (ADR 14);
`internal/pgstore` serves both halves.

Assets: A1, A3, A4, A5, A6.

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-STORE-1 | S, E | Attacker on the network connects to PostgreSQL, or Bearing connects as a role that can do more than own its schema | C-STORE-2, C-STORE-3 |
| T-STORE-2 | I | Store password leaks through a URL, process list, log or an environment variable or file the driver reads | C-STORE-1 |
| T-STORE-3 | E, T | SQL injection through source values or settings | C-STORE-5 |
| T-STORE-5 | T, R | Someone with database access edits, deletes or prunes audit records to hide a change | C-AUDIT-2, C-AUDIT-3, C-AUDIT-4, C-AUDIT-5 |
| T-STORE-6 | I, S | Credentials sniffed on the store connection, or a man in the middle serves a copy of the store | C-STORE-6 |
| T-STORE-7 | I | A backup or export is stolen | C-STORE-7 |
| T-STORE-8 | T, E, D | A tampered, corrupt or truncated backup is restored as primary state, or a restore that fails halfway is used as if it were whole | C-STORE-8, C-AUDIT-1, C-API-4 |
| T-STORE-9 | D | A ChangeSet of many tiny items, one huge timeline or many merges stays under the byte limit but stalls the store | C-STORE-9 |

**Retired:** T-STORE-4 and C-STORE-4 covered SurrealDB's network functions and scripting;
PostgreSQL has no equivalent that a role without administrator rights can
reach, so they are retired and their numbers are not reused.

- **C-STORE-1** The store password comes only from the environment variable
  `BEARING_STORE_PASSWORD` (a secret reference, C-SECRET-1). `pkg/store`
  rejects a URL with a password in it or with a `password`, `passfile`,
  `service`, `servicefile` or `sslpassword` parameter, and rejects any
  parameter it does not name. `pgstore` builds the connection string
  itself with every key the driver would fill from `PG*` environment
  variables, the password file or a service file set explicitly, so none of
  those is read, and refuses to open while `PGSERVICE` or `PGSERVICEFILE` is
  set because a service file cannot be overridden. Store errors name a
  server only by host and port (or "the Unix socket in <directory>"), never
  the user, the database, the path or the query, and the driver's errors,
  which quote the connection string, are reduced to their cause, and the
  server's refusal of a role or database (which quotes the name) to its
  SQLSTATE.
- **C-STORE-2** Bearing connects as a login role that owns its schema and
  nothing else: not a superuser, not `CREATEROLE`, not `CREATEDB`. `Open`
  checks those three attributes and refuses a role that has one, unless
  `insecure_store_superuser=true` (C-GEN-1), and logs a warning when it is
  set. The check does not look at REPLICATION, BYPASSRLS or membership of
  the `pg_*_server_*` roles, so the operator's own review of the role
  still matters. `pgstore.Provision` creates
  such a role and a schema it owns, and CI runs the PostgreSQL suites as
  that role. The connection's `search_path` is Bearing's schema alone.
  pgvector is the only extension, and an administrator installs it: opening
  with `vector_dimensions` checks that it exists (version 0.5 or later) and
  never creates it, and the vector type and operators are named with the
  schema the extension lives in because the `search_path` holds Bearing's
  schema alone. A role that cannot create its own schema is told to have an
  administrator create it.
- **C-STORE-3** The compose deployment generates a random PostgreSQL
  password on first start into a secrets file (mode 0600), passes it as a
  Docker secret, and does not publish the PostgreSQL port. The store secret
  is mounted outside the Source secrets directory. Planned with the compose
  deployment.
- **C-STORE-5** Values reach SQL only as bound parameters; bulk writes bind
  arrays (`unnest`) rather than building statements. Role and schema names
  cannot be parameters in `CREATE ROLE` or `CREATE SCHEMA`, so they must
  match `^[a-z_][a-z0-9_]{0,62}$` before the server quotes them (`format`
  with `%I`; escaping alone is not enough), and a password is quoted by the
  server (`%L`). A name that does not match is refused without being
  repeated in the error.
  Vectors bind as text cast to the column type, after the store has checked
  their length and that every number is finite and that the squares of
  the elements sum, in float32 as pgvector computes them, to a finite number
  of at least 1e-30. The extension's schema, the one name in a vector statement that is
  not fixed, comes from the server's catalog and is quoted by the server
  (`%I`); nothing in the URL or a point reaches it.
- **C-STORE-6** Store connections use TLS with the server's certificate
  verified: `pkg/store` requires `sslmode=verify-full` for a host that is
  not loopback or a Unix socket, and `insecure_store_plaintext=true` is the
  only way around it. Compose will set it for its internal network, with no
  published port; `pkg/store` logs a warning when it is in effect, and
  `bearing status` will show it. A URL names one host: a comma-separated
  list is refused, because the driver would try each host with the same
  `sslmode`. A loopback or socket
  connection defaults to `sslmode=prefer`. The driver's minimum protocol
  version is pinned to 3.0 and `target_session_attrs=read-write` keeps the
  pool off a read-only replica. The store benchmark (`cmd/bearing-bench`, a
  developer tool that is not shipped) opens `pkg/store` first, so these
  checks apply to it, and then a second direct connection for sizes, plans
  and `VACUUM`; it reads the password only from the same variable.
- **C-STORE-7** Operator docs state that exports contain organization data
  and audit records, and must be stored encrypted. Exports never contain
  secrets (C-SECRET-2).
- **C-STORE-8** `GraphStore.Restore` works only into an empty store and
  leaves it empty on any failure. It verifies the backup's integrity trailer
  (record count and SHA-256 over every byte before it) and refuses an
  unknown format or version, a missing trailer and data after it
  (docs/spec/contracts.md, "Backup"). Every frame is at most
  `contracts.MaxChangeSetBytes`, and replaying a record applies the count
  limits too (`contracts.CheckChangeSetLimits`). The reference store replays its change
  journal, which regenerates merge reviews and un-merge records from the
  entries rather than reading them from the backup, and refuses any apply
  that decides differently, an entry with no
  record time or one later than the header's `taken_at`, which it doesn't
  compare with its own clock, so a backup from a host whose clock ran ahead
  restores with its head and ID timestamps ahead too. These checks are in
  the store contract today. **Planned**, with the restore entry point, which
  doesn't exist yet: that entry point caps the backup's total size; it
  reports the backup's `taken_at` and head against the host's clock and
  refuses, or asks for an audited confirmation, when they are further ahead
  than a configured margin; it is an admin operation (admin role only,
  C-API-4) never reachable from API or MCP input without these checks; and
  each restore is audited per C-AUDIT-1 with the actor and the backup's
  SHA-256. The SHA-256 catches corruption and truncation, not a forger who
  recomputes it or sets `taken_at`: the audited hash will let an operator
  compare the restored backup with the one they took.
- **C-STORE-9** `GraphStore.Apply` refuses a ChangeSet over `MaxChangeSetBytes`
  (by `proto.Size`) or over a count limit (`MaxChangeSetItems`,
  `MaxChangeSetMerges`, `MaxTimelineRows`; `contracts.CheckChangeSetLimits`,
  docs/spec/contracts.md) before it takes any write. A backend's work in
  Apply MUST depend on the ChangeSet and the subjects and aliases it names,
  not on the rest of the store. The reference store's tests apply, under a
  five-second bound, 250 merges and 250 un-merges against stores holding
  50,000 aliases, merges into aliases that have moved, and 400 full 256-row
  timelines, all changed. Merge records, which carry both alias sets, are
  refused as soon as together they pass the byte limit, and a record that
  grows past it when recorded is refused too. A limit error quotes at most
  64 bytes of the input. Audit entries are held to byte bounds per field
  (`MaxAuditIDBytes`, `MaxAuditReasonBytes`) because they are kept for good,
  and a merge record's reviews are capped at `MaxTimelineRows`, a full record
  replacing its newest review so that a source toggling evidence cannot make
  the events that touch the merge fail. `pgstore` meets the rule for merges:
  its component table names the merge component of every subject any merge
  record ever joined, and an operation loads the merge records of the
  components of the subjects it names and no others (#81); a test shows that
  each operation reads the same answers as the reference store while
  loading only those. Its Apply is one transaction under the head row's
  lock, so a stalled writer holds the queue for as long as the transaction
  lasts; bulk rows are written in chunks of 5,000, and the largest
  permitted ChangeSet is measured in #135. **Known exception:** an operation
  loads the whole history of each series it touches, and an unfiltered
  `Supports`, `AsOf`, `Changes` or `DataQuality` loads every series of its
  table. That stays until a series can be loaded as of one record time.
  Components have no size cap either: an operation that names one member of
  a very large component loads every merge record of it, and a merge that
  joins two components rewrites the labels of the larger. A cap with a clear
  error is a follow-up.
  Retries are bounded: an Apply that the database keeps failing (a
  deadlock, a serialization failure, a lost connection) gives up after six
  attempts with `ErrBusy`, having written nothing visible; the event's
  mark makes a repeat safe.
- **C-STORE-10** `internal/memstore`'s rule engine is trusted production
  code. The PostgreSQL backend runs every operation on it (rows are loaded
  into a scratch store and the delta written back), so a flaw in it is a
  flaw in every backend built on it, and changes to it are reviewed as store
  changes. The PostgreSQL tests compare `pgstore` with the engine itself on
  random histories, so a difference between the two shows up as a failure.

### B7. Operators and configuration

Operators are trusted, but mistakes are expected. Configuration comes from
files in Git (`bearing apply`) or the API (ADR 10).

Assets: A5, A2, A6, the container and host.

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-OPS-1 | T | A setting silently weakens security | C-GEN-1, C-GEN-2 |
| T-OPS-2 | I | Secret values committed to Git, or left in files other users can read | C-SECRET-1, C-SECRET-5 |
| T-OPS-3 | I | Secrets leak through logs, traces or errors | C-SECRET-2, C-SECRET-4 |
| T-OPS-4 | E | A config change widens an adapter's grant unnoticed | C-ADAPTER-2, C-OPS-1 |
| T-OPS-5 | T | Startup config directory writable by others | C-OPS-3 |
| T-OPS-6 | E | A compromised Bearing process escalates on the host | C-OPS-4 |
| T-OPS-7 | E | An admin points a Source's secret reference at Bearing's own credentials or a host file and sends it to an allowed host | C-SECRET-1 |

- **C-OPS-1** Config apply validates types, validation rules (C-GEN-3), adapter
  settings and grants (ADR 10). `bearing diff` shows grant changes
  separately from other changes.
- **C-OPS-2** Config changes need the `admin` role (or the local socket),
  are versioned and are audited with the actor (C-AUDIT-1, C-AUDIT-6).
- **C-OPS-3** `bearing server --config` warns if a config file or directory
  is group- or world-writable.
- **C-OPS-4** Container images run as a non-root user with a read-only root
  filesystem, `no-new-privileges` and all Linux capabilities dropped.

### B8. Build and release supply chain

Users run what the project ships. A compromise here bypasses every runtime
control. The default binary is CGO-free and contains no BSL-licensed code
(ADR 14). An embedded PostgreSQL build, the intended single-binary option,
needs its own section here before it ships.

Assets: source repository, CI, release binaries, container images, embedded
first-party adapters, the local embedding model, the project website
(GitHub Pages, built from `site/`).

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-SUPPLY-1 | T | Malicious or vulnerable dependency or build tool | C-SUPPLY-1 |
| T-SUPPLY-2 | T | Compromised GitHub Action or over-privileged CI token | C-SUPPLY-2 |
| T-SUPPLY-3 | T | Malicious pull request runs with secrets or lands unreviewed | C-SUPPLY-2, C-SUPPLY-3 |
| T-SUPPLY-4 | S, T | Users download a tampered binary or image | C-SUPPLY-4 |
| T-SUPPLY-5 | T | An image tag or model file is replaced upstream | C-SUPPLY-5 |
| T-SUPPLY-6 | T | A first-party adapter module differs from its source | C-SUPPLY-6 |
| T-SUPPLY-7 | T, I | Script injected into the project website, or build secrets published on it, through repository files or GitHub issue and milestone text | C-SUPPLY-2, C-SUPPLY-7 |

- **C-SUPPLY-1** Few dependencies (ADR 1): each new one is justified in its
  commit, significant ones need an ADR. CI runs `govulncheck` and builds
  with `-mod=readonly` against the committed `go.sum`. Build tools (buf
  remote plugins, TinyGo, the Rust toolchain) are pinned by version and
  checksum.
- **C-SUPPLY-2** Workflows have read-only default permissions, pin
  third-party actions by commit SHA, and never run fork code with secrets
  (no `pull_request_target` checkout of PR code). `id-token: write` and
  release secrets exist only in the release job, run from a protected tag
  and environment. The one exception is the Pages deploy job, which holds
  `id-token: write` and `pages: write` and nothing else, runs only on
  `main`, and deploys through the `github-pages` environment, whose
  deployment branches are limited to `main`. Pull requests never run on
  self-hosted runners.
- **C-SUPPLY-7** The website generator renders Markdown with raw HTML
  disabled and puts GitHub API text (issue and milestone titles, states,
  URLs) only through `html/template` auto-escaping, never into
  `template.HTML` or the search index. It reads repository files through
  `os.Root`, so a committed symlink can't pull files from outside the
  repository into the site, and writes only inside its output directory.
  The build job's token is read-only. The daily rebuild republishes issue
  text without review, which this escaping makes safe to do.
- **C-SUPPLY-3** `main` is protected: changes go through reviewed pull
  requests with passing CI.
- **C-SUPPLY-4** Releases are built in CI from a tag with `-trimpath`, and
  ship checksums, an SBOM, build provenance and Sigstore signatures for
  binaries and images.
- **C-SUPPLY-5** The compose file pins every image (Bearing, PostgreSQL with
  pgvector, embedding model server) and model file by digest.
- **C-SUPPLY-6** First-party adapters are compiled from this repository in
  the same build and embedded in the binary; they are never downloaded at
  run time.

### B9. Model providers

Embedding and language models see organization data and return untrusted
output.

Assets: A3 (text sent for embedding or extraction), A1 (through candidates),
provider API keys.

| ID | STRIDE | Threat | Controls |
| --- | --- | --- | --- |
| T-MODEL-1 | I | Organization data sent to a hosted provider without the operator knowing | C-MODEL-1, C-MODEL-2 |
| T-MODEL-2 | T | Model output creates or changes facts | C-SOURCE-5, C-MODEL-3 |
| T-MODEL-3 | I | Provider API key leaks | C-SECRET-1, C-SECRET-2 |
| T-MODEL-4 | I, S | The local embedding service is reached from outside or sends data out | C-MODEL-4 |

- **C-MODEL-1** Bearing calls no hosted LLM or embedding API by default.
  Compose runs a local embedding model.
- **C-MODEL-2** A hosted provider is enabled per provider with
  `insecure_hosted_model_<provider>` (C-GEN-1), and its config lists the
  fields sent. `bearing status` shows each enabled provider and its fields.
- **C-MODEL-3** Vector search only ranks candidates; results are verified in
  the graph before they are returned as answers (ADR 2).
- **C-MODEL-4** Compose's embedding service has no published port and no
  egress network.

## Accepted risks and out of scope for the MVP

- **The IdP is trusted.** Its administrators can grant any role by changing
  group membership. Bearing records the actor but cannot prevent this.
- **Tokens stay valid until they expire.** Bearing does not check
  revocation. Operators should configure short token lifetimes.
- **`read` sees the whole graph.** There are no per-entity permissions. A
  stolen agent credential can read everything an engineer can.
- **Agents with other tools.** An agent that reads injected source text
  through Bearing may misuse tools it has elsewhere. C-MCP-2 labels the text;
  what the agent does with it is the agent's responsibility.
- **The host is trusted.** Root, or any process running as the service user,
  can use the Unix socket, read secrets from the environment and change the
  store.
- **Direct database edits to facts.** Anyone with write access to the store
  can change facts. The audit chain (T-STORE-5) shows changes that bypass
  Bearing only where an audit record is expected and missing; it does not
  prevent them.
- **Source systems are trusted for what they report.** If GitHub says a team
  owns a repo, Bearing records it, with its source. Anyone who can push to a
  default branch can change ownership claims; that is how CODEOWNERS works.
- **Old replays.** A delivery replayed after its processed ID has been
  pruned can be applied again. C-INGEST-5's update-time rule and the next
  sync limit the effect.
- **Data in allowed requests.** A module can encode data in GET requests to
  hosts it is allowed to call.
- **Erasure requests.** The audit log is append-only and raw events are kept
  30 days by default (ADR 7), so a request to erase a person's data cannot
  be met fully before retention removes them. Audit records name people by
  stable ID only.
- **Local-process adapters** run as the service user by default, outside
  the sandbox, so a malicious one is equivalent to the host (see B2).
  C-ADAPTER-2, 7, 11 and 16 and C-SECRET-3's "no others" limit what a
  correct adapter receives and what the core accepts from it, not what a
  malicious one can do. Operators run only local-process adapters they
  would run as the service user. A separate unprivileged UID per adapter
  is a planned hardening option.
- **WASM side channels** (timing, speculative execution) between modules in
  one process are not addressed.
- **Encryption at rest** is left to the disk or volume.
- **Telemetry backends** are the operator's to secure. Telemetry carries
  entity names and IDs, never secrets or tokens.
- **Volumetric denial of service** is left to the network or a proxy in
  front of ingest and the API.
- **Out of scope:** distributed mode (remote adapters, remote capability
  providers, NATS or Kafka), the `Executor` and any write action against
  source systems, an embedded PostgreSQL build, and Sigstore verification of
  external modules. Each needs its own section here before it ships.
