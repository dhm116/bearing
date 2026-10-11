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
| TCP | Everyone else | An OIDC bearer token in the `Authorization` header (never a query parameter), checked as ADR 12 describes | The roles the token's groups or client ID map to |

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

A request is checked in this order, and the first failure is the answer:

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
store: the same answers, with the same errors. `--server` is the path of the
Unix socket, which needs no token, or `https://host:port` with a bearer token
read from the variable `--token-env` names (default `$BEARING_TOKEN`), never a
value on the command line. `--server-ca` adds a certificate authority to trust.
Plain `http://` is accepted for a loopback address only, the token is never sent
anywhere else in clear, and a redirect is not followed. A flag beats a variable,
the two flags together are refused, and `$BEARING_SERVER` beats `$BEARING_STORE`.

`bearing source sync <source> --server ADDR` makes the sync request and prints
the request's event ID and offset; the sync itself runs later.

## Not here yet

Webhook deliveries (`POST /webhooks/{source}`, authenticated by the source's
signature, not a token), the metrics listener and configuration routes.
