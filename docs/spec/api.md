# Server API

How a caller reaches a running `bearing server`: the routes, who may use
each, and what the answers look like. Draft, like the rest of the
specification; the routes are under `/v1` until the MVP closes. Every
authenticated caller may call every route; there are no roles yet.

## Listeners

| Listener | For | Authentication |
| --- | --- | --- |
| Unix socket | Callers on the server's host, such as `bearing` on the command line | The kernel's report of the peer's UID: only the service user and root are served, any other peer is closed before it sends anything. The caller is recorded as `local:<uid>`. |
| TCP | Everyone else | The API token as a bearer token in the `Authorization` header (never a query parameter), compared in constant time with the value of the environment variable that `auth.token_env` names; except `POST /webhooks/{source}`, which proves itself by its source's signature ([Webhook deliveries](#webhook-deliveries)) |

The socket's directory MUST be mode 0700 exactly and owned by the service
user, and the socket is mode 0600; the server refuses to start otherwise. It is
available on Linux only for now, and elsewhere it refuses every connection.
The TCP listener is off unless `auth.token_env` is set. The server refuses to
start if that variable is empty, holds fewer than 32 bytes, or holds
whitespace or control characters. The listener uses TLS 1.2 or later;
plaintext is accepted only when the listen address is a literal loopback IP
(`127.0.0.1`, `[::1]`; the name `localhost` does not count, because a
resolver can point it anywhere), or on another address with
`listen.insecure_api_plaintext`, which logs a warning at start. `GET /healthz` and `GET /readyz` need no
credentials on either listener and return a status code and one word; they do
not count against the request limits and carry no request ID.

## Requests

A request to the API (not a webhook delivery) is checked in this order, and the
first failure is the answer:

1. The route exists (else 404 `not_found` or 405 `method_not_allowed`, with
   an `Allow` header) and, unless the route takes a body, the request has none
   (else 400 `invalid_request`, whatever the body's size or framing); a body
   that announces more than 8 KiB is a 413 `too_large`. Nothing else is looked
   at before this.
2. The server is below its limit of requests in flight (else 503 `busy`). The
   request's deadline starts here.
3. Authentication (else 401 `unauthenticated`).
4. The caller's rate limit (else 429 `rate_limited`).
5. For a sync request, that the source exists (else 404 `unknown_source`).
6. For a slow read (`changes`, `find`, `links`, and `related` with `depth`
   above 1), the caller's slow reads in flight (else 429 `too_many_reads`).
7. The parameters or, for a decision, the caller (an agent is a 403
   `forbidden`) and the body (else 400 `invalid_request`).

Parameters go in the query string. A name the route does not define, a name
given twice, text with control characters and anything over its length limit
is a 400. Times are RFC 3339. `find` with a `kind` that is not a kind is a 400.

## Routes

| Route | Method | Parameters | Answer |
| --- | --- | --- | --- |
| `GET /v1/get` | `query.get` | `ref` (required), `at` | The entity: its kind, name, keys and the facts about it |
| `GET /v1/owner` | `query.owner` | `ref` (required), `at` | Who owns it: asserted `owned_by` facts, or, when there are none, asserted `approves_changes` facts (CODEOWNERS). Each fact keeps its predicate |
| `GET /v1/related` | `query.related` | `ref` (required), `predicate`, `direction` (`out`, the default, or `in`), `depth` (1 to 10, default 1), `at` | What it relates to: `out` follows facts from it (what it depends on), `in` follows facts to it (what depends on it). Each step has its depth and fact |
| `GET /v1/changes` | `query.changes` | `ref`, `since`, `until`, `limit` (1 to 1,000) | Facts opened or closed in the window, newest first |
| `GET /v1/find` | `query.find` | `q` (required), `kind`, `limit` (1 to 100, default 20) | `{"matches": [...]}`: entities whose name or keys resemble `q`, best first, each with a score from 0 to 1 |
| `GET /v1/links` | `links.list` | `status` (`proposed` or `asserted`), `kind` (`same_as` or `distinct_from`), `entity` (an entity ID, record ID or key), `limit` (1 to 1,000, default 100) | `{"links": [...]}`: the open identity links, strongest first. This is the review queue when `status=proposed`. Each link has its `id`, `kind`, `status`, `rule`, both records (`a`, `b`: `id`, `kind`, `name`, first `key`, `sources`), its `score` (`strength`, `certainty`, `support`, `contrary`) at the server's time, and its `deposits`: the evidence over time with a matcher's `features`, or, for a person's decision, the `actor` and `reason` |
| `POST /v1/links/decisions` | `links.decide` | JSON body (below) | 202 `{"event_id", "offset"}`, only after the decision is on the event log (partition `core/identity`); the worker applies it in order, once. 400 for a malformed or inconsistent decision, 404 for a link or record that does not exist, 403 for an agent, 501 when the server takes no decisions |
| `POST /v1/sources/{source}/sync` | `sync.request` | none | 202 `{"event_id", "offset"}`, only after the request is on the event log. `event_id` names the request; `offset` is its position in the source's partition of the log. No route reads either back yet. While a source's last sync request is not yet applied, another request for it is not logged: the answer is the same 202 with the pending request's `event_id` and `offset`, so a loop of requests cannot fill the log. The pending request may already be running, in which case a change made after it started is seen by the next sync, not by this one |

`ref` is an entity ID or a key such as `github:repo/acme/payments`, at most
1,024 bytes. The answers are the JSON of the `pkg/query` types, the same ones
the command line prints; their field names are the wire format, and within
`/v1` fields are only added. `at` is an RFC 3339 time and asks what held
then; without it the answer is what holds now. With no `since`, `changes`
asks about the last 24 hours. Every fact carries its sources: the
observations behind it, each with its source, observed time and link:
per source the first and the latest, with `source_count` saying how many
there are in all, counted per fact row: where several rows are merged into
one fact the counts are summed, so one observation may be counted twice. A
relation carries `qualifiers`, how it holds (for a dependency: protocol,
port, service and where it was found). When
several sources say the same thing, the answer is one fact with all of
them.

**Who asked.** A sync request is recorded with the caller the authenticator
proved: `token` for the API token, or `local:<uid>` on the socket. Nothing in the request chooses it.

**Link decisions.** The body of `POST /v1/links/decisions` is one JSON object
(unknown fields are refused): `decision`, one of `accept`, `reject`, `join`,
`split` and `undo_distinct`, and either `link_id` (`accept` and `reject` a
proposed `same_as`, `undo_distinct` a `distinct_from`) or `a` and `b` (`join`
and `split`: two references, each an entity ID, a record ID or a key), and an
optional `reason`, free text clipped to 1 KiB and never interpreted. `accept`
asserts the proposed link; `reject` closes it and asserts a `distinct_from`, so
the pair is not proposed again; `join` asserts a `same_as` (and closes a
`distinct_from` between the pair); `split` closes the `same_as` links between
the pair and asserts a `distinct_from`; `undo_distinct` closes a
`distinct_from`. Facts stay on their records, so every decision can be undone
by another. The decision is recorded with the caller as its actor, and its
event ID is the key that makes applying it once: accepting a link that is
already asserted or closed changes nothing and logs a warning, and a decision
the store cannot carry out (a link that has vanished, records of different
kinds) becomes a dead letter with the reason.

Only a person decides ([ADR 7](../adr/0007-records-identity-links-matchers.md):
a proposed link becomes asserted only by a person or a rule the settings
name). An authenticator that knows its caller is an agent sets `Caller.Agent`,
and such a caller reads but gets a 403 on this route. Neither authenticator
the server has can tell yet: the API token and the socket's UID are people by
definition, so a person who hands the token to an agent hands over the right to
decide too. Telling an agent from a person needs an identity provider that
marks agents (OIDC); until then, hold the token as you would a password.

## Limits

Before any of the checks above, the TCP listener limits each remote peer (an
IPv6 peer is its /64): at most 64 open connections, 1,024 in all, closed as
they are accepted and before the TLS handshake, and 1,200 requests a minute
with a burst of 200 (else 429 `rate_limited`, counted whatever the token). It
speaks HTTP/1.1 only, so a connection carries one request at a time. The health
endpoints and the webhook route are not counted (the route has its own limit,
see [Webhook deliveries](#webhook-deliveries)), and the socket has no such
limit. Each caller has a request rate (default 600 a minute), at most two slow
reads in flight, and a deadline per request (30 seconds; 15 for a slow read).
The slow reads are `changes`, `find`, `links` and `related` with `depth` above 1: they
scan or walk many rows, so they share one cap and the shorter deadline. A
caller is its token subject at a peer address (an IPv6 peer is its /64), or
`local:<uid>` on the socket, so one remote client cannot use up another's
share of a shared token. The server holds at most 256 requests at once, reads
headers for at most 10 seconds and 16 KiB, and refuses any body except the 8 KiB
of a decision.

## Settings

The listeners come from the `listen` and `auth` blocks of the settings file
(`pkg/server/boot/settings.go`):

| Setting | Meaning |
| --- | --- |
| `listen.socket` | Path of the Unix socket |
| `listen.addr` | TCP address; needs `auth.token_env` |
| `listen.tls_cert`, `listen.tls_key` | PEM files for TLS; both or neither |
| `listen.insecure_api_plaintext` | Allow plain HTTP on a non-loopback address, for a TLS-terminating proxy |
| `listen.max_conns`, `listen.max_conns_per_peer` | Connection caps (defaults 1,024 and 64) |
| `listen.peer_rate_per_minute` | Requests a minute per peer before authentication (default 1,200) |
| `listen.insecure_no_peer_rate` | Turn the per-peer request limit off |
| `auth.token_env` | Name of the variable that holds the API token |

A limit that is present must be positive. The TLS certificate and key are
read once at start; changing them needs a restart.

## Errors

Every error is JSON with a `code` a program can switch on, a `message`
written for the caller and a `request_id`, which is also the `X-Request-Id`
header. Details go to the server's logs, never the answer. An answer that
asks the caller to try again (429, `busy` and `unavailable`)
carries `Retry-After`, in seconds.

| Status | `code` |
| --- | --- |
| 400 | `invalid_request` |
| 401 | `unauthenticated` |
| 403 | `forbidden` (an agent decided a link) |
| 404 | `not_found`, `unknown_source` |
| 405 | `method_not_allowed` |
| 413 | `too_large` |
| 429 | `rate_limited`, `too_many_reads` |
| 500 | `internal` |
| 501 | `not_implemented` (the server takes no link decisions) |
| 503 | `busy`, `unavailable` (the event log refused a sync request or a decision) |
| 504 | `timeout` |

## The command line

`bearing get`, `owner`, `related`, `changes` and `find` take `--server` (or
`$BEARING_SERVER`) instead of `--store`, and print what they print from a
store: the same answers, and errors of the same kinds: not found and invalid
request match `query`'s with `errors.Is`, and the exit codes
are the same. The server's messages are generic and carry a request
ID, so their text differs from a store's; the command line adds what it knows.
A not-found names what was asked about (`nothing matches
github:repo/acme/x`), and an unknown
source names the source. A 401 from the API adds a hint that names the variable
the token came from (never its contents): the server did not accept the token
in `$BEARING_TOKEN` (or the `--token-env` name). On the socket, which uses no token, the hint says that instead.

`--server` is the absolute path of the Unix socket (or `unix:///path`), which
needs no token, or `https://host:port` with a bearer token read from the variable `--token-env` names (default `$BEARING_TOKEN`), never a
value on the command line. `--server-ca` adds a certificate authority to trust.
Plain `http://` is accepted for a literal loopback address only (`127.0.0.1`,
`[::1]`; not a name such as `localhost`), the token is never sent anywhere
else in clear, and a redirect is not followed. A flag beats a variable,
the two flags together are refused, and `$BEARING_SERVER` beats `$BEARING_STORE`.

`bearing source sync <source> [--server ADDR] [--json]` takes the same
`--server`, `--token-env` and `--server-ca` flags and `$BEARING_SERVER`. It
makes the sync request and prints the request's event ID and offset (or them as
JSON); the sync itself runs later.

`--json` through `--server` prints the body the server sent, indented and with
no field dropped, so a field a newer server adds shows at once; the text
output shows the fields the client knows. In every `--json` output of the
command line, a store's too, text that a terminal would act on is written as a
JSON `\uXXXX` escape: DEL, the C1 controls (U+0080 to U+009F, which include the
8-bit CSI and OSC), format characters such as bidi overrides, and the line and
paragraph separators. The document means what it meant (a JSON reader decodes
the escape to the same character); only what a terminal would print differs. The guarantee is for a terminal in
UTF-8 mode; one that is not still reads parts of ordinary non-ASCII text as
controls (#284).
An answer that would indent
to more than 64 MiB (a deeply nested one) is printed compact, and one that is
not a JSON object is refused.

## Webhook deliveries

`POST /webhooks/{source}` is served by the TCP listener only, never the Unix
socket, and ahead of the API's token check. Because that listener needs the API
token, a server that takes webhooks has one configured too: a delivery is authenticated by its
source's signature, and everything else about it (body limit, rate limits,
answers with a status and a request ID only, 2xx only once the event is on the
log) follows the ingest rules in `pkg/server`. Everything
under `/webhooks/` is the ingest handler's and gets its answers, which are a
status code and a request ID with no body, not the API's JSON error: a POST to
a path that is not a configured source is a 401, as for a bad signature, any
other method is a 405 with `Allow: POST`, and more than the in-flight cap (256)
at once is a 503 with `Retry-After`. Every other path, `/webhooks` with no slash
included, is the API's, with all of its checks.

## Not here yet

The metrics listener and configuration routes.
