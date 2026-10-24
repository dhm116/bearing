# Threat model

Who and what Bearing trusts, where input enters, and which controls stand
between the two. One page, for the code on `pivot`; the design before the
restart is at the [`pre-pivot`](https://github.com/dhm116/bearing/blob/pre-pivot/docs/security/threat-model.md)
tag. To report a vulnerability, see [`SECURITY.md`](../../SECURITY.md).

Control IDs (`C-<AREA>-<n>`) are cited in code comments. Every ID in this
file is defined here, and an ID in code that is not defined here is a bug in
one or the other. The numbers are not consecutive: they keep the numbering
the code already uses.

## What is protected

- **The graph**: what Bearing says about owners and dependencies. Wrong data
  is as bad as leaked data.
- **Source credentials**: the tokens adapters use to read GitHub, Kubernetes
  and the directory.
- **The server's own secrets**: the API token, webhook secrets, TLS keys.

## Trust boundaries

| Boundary | Who is on the other side | What stands between |
| --- | --- | --- |
| API Unix socket | A process on the server's host | The kernel's report of the peer's UID: only the service user and root are served (C-API-1) |
| API TCP listener | Anyone who can reach the port | TLS, a bearer token of at least 32 bytes, per-peer connection and request limits before the token is checked (C-API-2, C-API-3, C-API-6) |
| Webhook route (`POST /webhooks/{source}`) | A source system, or anyone pretending to be one | The source's HMAC signature, checked before the body is parsed or logged (C-INGEST-1, C-INGEST-2) |
| Local adapter | A process the server starts | A private socket directory (mode 0700) and an environment the server builds (C-SECRET-3) |
| Remote adapter | A process on another host | Mutual TLS 1.3, each side's certificate from a CA it trusts and, optionally, holding the SPIFFE ID it expects (below) |
| Settings file and adapter commands | An operator | File ownership and mode checks at start (C-OPS-3) |

Adapter output is untrusted even from first-party adapters: the server
validates every observation with `pkg/model` before it is stored.

Adapters are trusted code. They run as the service user's child processes,
not in a sandbox (WASM sandboxing is deferred, see the roadmap), and they are
read-only against their source by convention, not by enforcement.

## Remote adapters

The core and a remote adapter each present a certificate that the other
checks against a CA it was configured with, never the system roots, over
TLS 1.3. There is no shared secret. Bearing issues no certificates: SPIRE
(through `spiffe-helper`) or any other CA does
([ADR 9](../adr/0009-spiffe-compatible-mtls.md)).

Without a peer ID, any certificate the CA issues is accepted, so the
adapter's client CA must issue only Bearing core certificates (any of them
can call `Sync` and `Handle`). With a peer ID (`tls.peer_id` on the server,
`BEARING_ADAPTER_TLS_PEER_ID` on the adapter), a shared CA such as a SPIRE
trust domain is safe: each side also requires the other's certificate to
hold exactly one URI SAN, the SPIFFE ID it expects. The core then checks the
ID in place of a host name, verifying the chain itself (C-ADAPTER-1).

Both sides re-read their CA, certificate and key when a file changes, at
most once a second, so short-lived certificates rotate without a restart. A
file that fails to load (half written, unsafe mode) leaves the last good one
in use and logs a warning naming the setting and path, never the contents.
The server refuses a CA, certificate or key file that group or others can
write, or that someone other than the service user or root owns, and every
load, first or reload, refuses a key others can read.

## What an adapter is given

The server builds an adapter's whole environment (C-SECRET-3): a fixed
`PATH` (`/usr/bin:/bin`), the secrets its source names, and the variables the
operator allowed for that adapter. The webhook secret, the store password and
everything else the server started with stay behind. Loader and runtime
variables (such as `LD_PRELOAD`, `GODEBUG`) and `BEARING_*` names are refused.

Secrets reach an adapter as environment variables, named by the key under the
source's `secrets`. This is a deliberate choice for now: it is simple and
needs no files, but anyone who can read the adapter's process environment
(the same user, or root) can read them. A remote adapter holds its own
credentials; the server never sends it any.

## Controls

**Remote adapters**

- **C-ADAPTER-1** With a peer ID set, a certificate must hold exactly one URI
  SAN, equal to it byte for byte as written in the certificate (so `#`, `?`
  or user info that parsing would drop is refused). Neither side accepts a
  peer certificate marked as a CA. The core verifies the adapter's chain against its CA
  (intermediates from the handshake, server-auth usage, the current time)
  before the ID, since it turns off crypto/tls's host name check. A peer ID
  that is not a SPIFFE ID, or one given with a server name, stops the start
  without echoing the value.
- **C-ADAPTER-2** TLS files are re-read when their inode, size or modification
  time changes. At start and on every reload, in the core and in the
  adapter, each file is opened without blocking and checked on the open
  descriptor: a regular file, not writable by group or others, a key not
  readable by them, at most 1 MiB, and owned by the process's user or root.
  A failed reload keeps the last good files, is retried, and is logged once
  per change of the files; other handshakes go on with the last good files
  while one reloads. The adapter issues no session tickets, so a session
  from before a CA rotation cannot be resumed after it.
  The directories holding the files are not checked: they must be writable
  only by the service user or root, or a symlink there can point at any
  root-owned file (the system trust bundle, for one).

**API**
- **C-API-1** The socket's directory is mode 0700 and the service user's, the
  socket is 0600, and a peer whose UID is neither the service user's nor
  root's is closed before it sends anything. The caller is recorded as
  `local:<uid>`.
- **C-API-2** The TCP listener is off unless `auth.token_env` names a variable
  holding a token of at least 32 bytes without whitespace or control
  characters. There is no setting for open access. The token is compared in
  constant time.
- **C-API-3** The TCP listener uses TLS 1.2 or later. Plaintext is allowed only
  on a literal loopback IP, or elsewhere with `insecure_api_plaintext`, which
  logs a warning. The command line sends the token only over TLS or to a
  literal loopback IP and follows no redirect.
- **C-API-6** Limits before and after authentication: connection caps per peer
  and in all (closed before the TLS handshake), a per-peer request rate, a
  per-caller rate, a lower per-caller rate for link decisions (30 a minute),
  a cap on requests in flight, a deadline per request, and header and body
  limits: no route takes a body except a link decision, at most 8 KiB, read
  strictly (`application/json`, known keys once each). See
  [the API spec](../spec/api.md#limits).
- **C-API-7** Errors carry a code, a generic message and a request ID. Details
  go to the logs, never the answer.
- **C-API-8** Who may decide identity links is `api.link_decisions`: `socket`
  (the default) takes decisions only from callers on the Unix socket,
  `token` from API token holders too, `off` from no one. Anyone else gets a
  403 before the body is read, and a caller marked as an agent never decides.
  A decision is an assertion of identity that ownership answers then rest on,
  so by default only a person on the server's host makes one. The `reason`
  that comes with a decision is the caller's own text, which people and agents
  later read in `bearing links`; it is kept as given, never interpreted,
  clipped to 1 KiB, and refused if it holds control characters other than
  newline and tab.

**Ingest**

- **C-INGEST-1** The webhook route is served by the TCP listener only, ahead of
  the token check, and answers 2xx only after the event is on the log (503 if
  the log refuses). Sync requests follow the same rule.
- **C-INGEST-2** The server, not the adapter, checks the delivery's HMAC-SHA256
  signature in constant time, with the source's secret, before anything else
  is stored. The adapter never sees the secret.
- **C-INGEST-4** A delivery is bounded: body size (default 1 MiB), deliveries in
  flight (default 256), and the headers kept (64 headers, 16 KiB).
- **C-INGEST-5** A delivery's event ID is the SHA-256 of its body, so a repeat is
  a duplicate on the log and does nothing twice.
- **C-INGEST-7** Answers are a status code and a request ID, with no body. An
  unknown source is answered like a known one with a bad signature.
- **C-INGEST-8** Per-peer and per-source rate limits (default 1,200 a minute,
  burst 200) answer 429. The per-peer limit comes before the signature check.
- **C-INGEST-9** Nothing but the signature check looks at a delivery's body,
  and nothing is stored, until the signature has been verified.
- **C-INGEST-10** `X-Forwarded-For` and similar headers are ignored: the peer is
  the connection's address, because a forwarded header is the sender's claim.
- **C-INGEST-11** What is logged of a delivery leaves out `Authorization`,
  `Proxy-Authorization`, `Cookie` and `Set-Cookie`, and error text is cleaned
  before it is logged or stored.
- **C-INGEST-12** The apply workers read each partition in order. An event that
  keeps failing is retried with a growing pause and then recorded as a dead
  letter, so one bad event does not stop the others.

**Secrets**

- **C-SECRET-1** A field that takes a secret takes only an `env:NAME` reference,
  never a value. `NAME` must match the allowlist (default `BEARING_SECRET_*`),
  which cannot cover the server's own variables.
- **C-SECRET-2** A reference that does not resolve stops the start. The error
  names the field and the variable, never a value.
- **C-SECRET-3** An adapter gets only the environment described above.

**Operations and store**

- **C-OPS-3** The settings file and each adapter command must be owned by the
  service user or root and not writable by others, because write access to
  either is code execution. The check runs at start; a restart does not
  repeat it.
- **C-OPS-5** An adapter that fails to start or answer is restarted with a pause
  that doubles from one second to one minute, so a broken adapter cannot spin.
- **C-STORE-9** Reads that scan or walk many rows (`changes`, `find`,
  `links`, and `related` deeper than one hop) share a per-caller cap and a
  shorter deadline. `links` scores at most the 5,000 links with the newest
  evidence.
- **C-STORE-11** Trim removes only events that are not marked to keep and that
  every required group has applied, so a stopped worker never loses work. The
  `bearing.worker.oldest_unapplied.age` metric shows it falling behind.

**Secrets never appear in** config values, URLs, logs, span attributes, metric
labels or error text. Tests plant canary values and scan for them
(`internal/testkit`).

## Known gaps

Each is in [`edge-cases.md`](../edge-cases.md) with an example and a fix.

- **No roles or per-caller identity.** Every authenticated API caller may call
  every route but link decisions (C-API-8), and every holder of the API token
  is the same caller. With `api.link_decisions: token`, anyone holding the
  token, an agent included, can assert or break an identity link, and the
  decision is attributed to `token`, not to a person.
- **The API's TLS material is read once.** A renewed API certificate needs a
  restart. (A remote adapter's files are reloaded.)
- **Secret volumes.** A Kubernetes Secret mounted with the default mode 0644 is
  refused until `defaultMode` is 0400.
- **Handshake failures are logged at Debug only.** A TLS scan is bounded by the
  connection caps but is not visible at the default log level.
- **Webhook replay** after the log's retention has dropped the first copy is
  applied again.
- **A short window** exists between creating an adapter's socket and setting
  its mode (its directory is already private).
- **Remote adapters** are redialled without a backoff, and the adapter
  protocol version is not part of the wire exchange.
