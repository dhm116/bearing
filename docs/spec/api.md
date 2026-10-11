# Server API

How a caller reaches a running `bearing server`: the routes, who may use
each, and what the answers look like. Draft, like the rest of the
specification; the routes are under `/v1` until the MVP closes. Roles and
the decision behind them are in the [Authorizer](contracts.md#authorizer)
section of the contracts, and the controls in the threat model
(C-API-1 to C-API-7).

## Listeners

| Listener | For | Authentication | Role |
| --- | --- | --- | --- |
| Unix socket | Callers on the server's host, such as `bearing` on the command line | The kernel's report of the peer's UID: only the service user and root are served, any other peer is closed before it sends anything. The caller is recorded as `local:<uid>`. | `admin` |
| TCP | Everyone else | An OIDC bearer token in the `Authorization` header (never a query parameter), checked as ADR 12 describes; except `POST /webhooks/{source}`, which proves itself by its source's signature ([Webhook deliveries](#webhook-deliveries)) | The roles the token's groups or client ID map to |

The socket's directory MUST be mode 0700 exactly and owned by the service
user, and the socket is mode 0600; the server refuses to start otherwise. It is
available on Linux only for now, and elsewhere it refuses every connection.
The TCP listener is off unless OIDC is configured and uses TLS 1.2 or later;
plaintext is accepted on a loopback address only, or on another with an
explicit insecure-plaintext setting (named `insecure_api_plaintext` when
configuration lands). `GET /healthz` and `GET /readyz` need no
credentials on either listener and return a status code and one word; they do
not count against the request limits and carry no request ID.

## Requests

A request to the API (not a webhook delivery) is checked in this order, and the
first failure is the answer:

1. The route exists (else 404 `not_found` or 405 `method_not_allowed`, with
   an `Allow` header) and the request has no body (else 400 `invalid_request`,
   whatever the body's size or framing). Nothing else is looked at before this.
2. The server is below its limit of requests in flight (else 503 `busy`).
3. Authentication (else 401 `unauthenticated`, or 503 `auth_unavailable` when
   the identity provider's keys cannot be had).
4. The caller's rate limit (else 429 `rate_limited`).
5. For a sync request, that the source exists (else 404 `unknown_source`).
6. Authorization of the route's method (else 403 `forbidden`, or 503
   `auth_unavailable` if the authorizer cannot decide).
7. For `changes`, the caller's reads in flight (else 429 `too_many_reads`).
8. The parameters (else 400 `invalid_request`).

Parameters go in the query string. A name the route does not define, a name
given twice, text with control characters and anything over its length limit
is a 400. Times are RFC 3339.

## Routes

| Route | Method (role) | Parameters | Answer |
| --- | --- | --- | --- |
| `GET /v1/get` | `query.get` (`read`) | `ref` (required), `valid_at`, `recorded_at` | What is known about a subject: its keys, facts, conflicts and merges |
| `GET /v1/owner` | `query.owner` (`read`) | `ref` (required), `valid_at`, `recorded_at` | Who owns it, from asserted facts only |
| `GET /v1/related` | `query.related` (`read`) | `ref` (required), `predicate`, `valid_at`, `recorded_at` | What it relates to, in both directions |
| `GET /v1/changes` | `query.changes` (`read`) | `ref`, `since`, `until`, `axis` (`valid` or `record`), `limit` (1 to 1,000), `page_token` | One page of the facts that changed in a window, newest first, with `next_page_token` while more follow |
| `POST /v1/sources/{source}/sync` | `sync.request` (`ingest` for that source) | none | 202 `{"event_id", "offset"}`, only after the request is on the event log. `event_id` names the request; `offset` is its position in the source's partition of the log. No route reads either back yet |

`ref` is a subject ID or a key such as `github:repo/acme/payments`, at most
1,024 bytes. The answers are the JSON of the `pkg/query` types, the same ones
the command line prints; their field names are the wire format, and within
`/v1` fields are only added. With no `since` or `until`, `changes`
asks about the last 24 hours.

**Page tokens.** A token carries the whole question and is not signed. It is
input, like any other: the request that carries it is authenticated and
authorized as a request without one, on every page, and a token cannot be
combined with `ref`, `since`, `until` or `axis`. A caller whose role is
withdrawn between two pages is refused on the second.

**Who asked.** A sync request is recorded with the caller the authenticator
proved: the token's subject, a client's client ID (marked as an agent), or
`local:<uid>`. Nothing in the request chooses it.

## Limits

Each caller has a request rate (default 600 a minute), at most two `changes`
reads in flight, and a deadline per request (30 seconds; 15 for `changes`,
which scans fact rows). The server holds at most 256 requests at once, reads
headers for at most 10 seconds and 16 KiB, and refuses any body (no route
takes one).

## Errors

Every error is JSON with a `code` a program can switch on, a `message`
written for the caller and a `request_id`, which is also the `X-Request-Id`
header. Details go to the server's logs, never the answer. An answer that
asks the caller to try again (429, `busy`, `auth_unavailable` and `unavailable`)
carries `Retry-After`, in seconds.

| Status | `code` |
| --- | --- |
| 400 | `invalid_request`, `invalid_page_token` |
| 401 | `unauthenticated` |
| 403 | `forbidden` |
| 404 | `not_found`, `unknown_source` |
| 405 | `method_not_allowed` |
| 429 | `rate_limited`, `too_many_reads` |
| 500 | `internal` |
| 503 | `busy`, `auth_unavailable`, `unavailable` (the event log refused a sync request) |
| 504 | `timeout` |

## The command line

`bearing get`, `owner`, `related` and `changes` take `--server` (or
`$BEARING_SERVER`) instead of `--store`, and print what they print from a
store: the same answers, and errors of the same kinds: not found, invalid
request and bad page token match `query`'s with `errors.Is`, and the exit codes
are the same. The server's messages are generic (C-API-7) and carry a request
ID, so their text differs from a store's. `--server` is the absolute path of
the Unix socket (or `unix:///path`), which needs no token, or `https://host:port` with a bearer token
read from the variable `--token-env` names (default `$BEARING_TOKEN`), never a
value on the command line. `--server-ca` adds a certificate authority to trust.
Plain `http://` is accepted for a literal loopback address only (`127.0.0.1`,
`[::1]`; not a name such as `localhost`), the token is never sent anywhere
else in clear, and a redirect is not followed. A flag beats a variable,
the two flags together are refused, and `$BEARING_SERVER` beats `$BEARING_STORE`.

`bearing source sync <source> [--server ADDR] [--json]` takes the same
`--server`, `--token-env` and `--server-ca` flags and `$BEARING_SERVER`. It
makes the sync request and prints the request's event ID and offset (or them as
JSON); the sync itself runs later. `--json` through `--server` prints the
fields this client's types know, so a field a newer server adds shows only once
the client is updated.

## Webhook deliveries

`POST /webhooks/{source}` is served by the TCP listener only, never the Unix
socket, and ahead of the API's token check. Because that listener needs OIDC
(C-API-2), a server that takes webhooks has OIDC configured too: a delivery is authenticated by its
source's signature, and everything else about it (body limit, rate limits,
answers with a status and a request ID only, 2xx only once the event is on the
log) is the ingest rules of the threat model (C-INGEST-2 to C-INGEST-11). Everything
under `/webhooks/` is the ingest handler's and gets its answers, which are a
status code and a request ID with no body, not the API's JSON error: a POST to
a path that is not a configured source is a 401, as for a bad signature, any
other method is a 405 with `Allow: POST`, and more than the in-flight cap (256)
at once is a 503 with `Retry-After`. Every other path, `/webhooks` with no slash
included, is the API's, with all of its checks.

## Not here yet

The metrics listener and configuration routes.
