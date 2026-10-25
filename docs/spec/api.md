# Server API

How a caller reaches a running `bearing server`: the routes, who may use
each, and what the answers look like. Draft, like the rest of the
specification; the routes are under `/v1` until the MVP closes. Every
authenticated caller may call every route except link decisions, which
`api.link_decisions` grants; there are no roles yet.

## Listeners

| Listener | For | Authentication |
| --- | --- | --- |
| Unix socket | Callers on the server's host, such as `bearing` on the command line | The kernel's report of the peer's UID and PID: only the service user and root are served, and not a process the server started (a local adapter) or its child; any other peer is closed before it sends anything. The caller is recorded as `local:<uid>`. |
| TCP | Everyone else | The API token as a bearer token in the `Authorization` header (never a query parameter), compared in constant time with the value of the environment variable that `auth.token_env` names; except `POST /webhooks/{source}`, which proves itself by its source's signature ([Webhook deliveries](#webhook-deliveries)) |

The socket's directory MUST be mode 0700 exactly and owned by the service
user, and the socket is mode 0600; the server refuses to start otherwise. It is
available on Linux only for now, and elsewhere it refuses every connection.
The server reads each caller's line of parents from `/proc`, so it must see
the processes there that the service user owns: with `hidepid` or systemd's
`ProtectProc=invisible`, processes of other users (such as `sudo`) may be
hidden, but the service user's own may not.
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
4. The caller's rate limit, and for a decision the caller's decision rate
   (else 429 `rate_limited`).
5. For a sync request, that the source exists (else 404 `unknown_source`).
6. For a slow read (`changes`, `find`, `links`, `runtimes`, and `related` with `depth`
   above 1), the caller's slow reads in flight (else 429 `too_many_reads`).
7. The parameters or, for a decision, the caller (one that
   `api.link_decisions` does not allow, or an agent, is a 403 `forbidden`),
   the content type (else 415 `unsupported_media_type`) and the body (else
   400 `invalid_request`).

Parameters go in the query string. A name the route does not define, a name
given twice, text with control characters and anything over its length limit
is a 400. Times are RFC 3339. `find` and `runtimes` with a `kind` that is not a kind are a 400.

## Routes

| Route | Method | Parameters | Answer |
| --- | --- | --- | --- |
| `GET /v1/get` | `query.get` | `ref` (required), `at` | The entity: its kind, name, keys and the facts about it |
| `GET /v1/owner` | `query.owner` | `ref` (required), `at`, `members` (`true`, or `false` or absent for none; any other value is 400 `invalid_request`) | Who owns it: asserted `owned_by` facts, or, when there are none, asserted `approves_changes` facts (CODEOWNERS). Each fact keeps its predicate. With `members=true`, `teams` also lists the people on each owning team that is not a person: `[{"team": Ref, "members": [{id, kind, name, key, role, since, sources}], "members_truncated"}]`. Members are the people with an asserted `member_of` fact to the team at `at`, maintainers first and then by name, at most 50 per team (`members_truncated` says more exist); a person in several sources is one entry with all their sources, and `role` is what a source gives (`maintainer` or `member` from GitHub; the directory gives none). A person who left is not listed. Without `members`, there is no `teams` field |
| `GET /v1/related` | `query.related` | `ref` (required), `predicate`, `direction` (`out`, the default, or `in`), `depth` (1 to 10, default 1), `at` | What it relates to: `out` follows facts from it (what it depends on), `in` follows facts to it (what depends on it). Each step has its depth and fact |
| `GET /v1/changes` | `query.changes` | `ref`, `since`, `until`, `limit` (1 to 1,000) | Facts opened or closed in the window, newest first |
| `GET /v1/find` | `query.find` | `q` (required, at most 256 bytes), `kind`, `limit` (1 to 100, default 20) | `{"matches": [...]}`: entities whose name or keys resemble `q`, best first, each with a score from 0 to 1 |
| `GET /v1/runtimes` | `query.runtimes` | `language` (required; `golang` means `go`, `nodejs` means `node`), `below` (a version such as `1.22`), `kind`, `at`, `limit` (1 to 1,000 entities per list, default 1,000) | Which entities declare a version of the language (`runtime` facts from manifests). With `below`: `below` lists entities with a declaration whose lowest allowed version is under it (a range such as `>=18` or `^20` counts as its lower bound), `unknown` those with none below and one that cannot be read (`lts/hydrogen`, `*`; never guessed), and `at_or_above` counts the rest; every entity shows all its declarations (`version`, `file`, `lower`, `is_below`, `is_unknown`, `sources`; see [Runtimes answer](#runtimes-answer)). Without `below`, `entities` lists every declaration. `truncated` says a list was cut at `limit`. 400 for a missing `language`, a `below` that is not a plain version, a `kind` that is not a kind, or a `limit` out of range |
| `GET /v1/links` | `links.list` | `status` (`proposed` or `asserted`), `kind` (`same_as` or `distinct_from`), `entity` (an entity ID, record ID or key; 404 when nothing has it), `limit` (1 to 1,000, default 100) | `{"links": [...]}`: the open identity links, strongest first (below). This is the review queue when `status=proposed` |
| `POST /v1/links/decisions` | `links.decide` | JSON body (below) | 202 `{"event_id", "offset"}`, only after the decision is on the event log (partition `core/identity`); the worker applies it in order, once. 400 for a malformed decision or one the store would refuse, 404 for a link or record that does not exist, 403 for a caller `api.link_decisions` does not allow or an agent, 415 for a body that is not `application/json`, 501 when the server takes no decisions |
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

**Links.** Each link in a `links` answer has these fields; those marked
*omitempty* are left out when empty:

| Field | Meaning |
| --- | --- |
| `id`, `kind`, `status`, `rule` | The link; `rule` names what made it: a key rule (`rule/<name>`), a matcher (`matcher/<name>`), ingest (`shared_key`, `linked_ids`) or `person` |
| `a`, `b` | Its records: `id`, and *omitempty* `kind`, `name`, `key` (the record's first key, sorted) and `sources` (the configured sources that observed it) |
| `score` | `strength` (0 to 1, the queue's order), `certainty` (0 to 1), `support` and `contrary` (decayed weights for and against), at the server's time |
| `deposits` | The evidence, oldest first: `at`, `weight`, and *omitempty* `against`, `observation_id`, `features` (a matcher's), `actor` and `reason` (a person's decision) |
| `valid_from` | When the link opened |

The queue is scored over at most the 5,000 links with the newest evidence;
since evidence decays, the strongest are among them.

**Link decisions.** The body of `POST /v1/links/decisions` is one JSON object
with the content type `application/json`, whose keys are exactly the field
names below, each at most once and each a string (anything else is a 400):
`decision`, one of `accept`, `reject`, `join`,
`split` and `undo_distinct`, and either `link_id` (`accept` and `reject` a
proposed `same_as`, `undo_distinct` a `distinct_from`) or `a` and `b` (`join`
and `split`: two references, each an entity ID, a record ID or a key), and an
optional `reason`, free text clipped to 1 KiB and never interpreted, with no
control characters other than newline and tab. `accept`
asserts the proposed link; `reject` closes it and asserts a `distinct_from`, so
the pair is not proposed again; `join` asserts a `same_as` (and closes a
`distinct_from` between the pair); `split` closes the `same_as` links between
the pair and asserts a `distinct_from`; `undo_distinct` closes a
`distinct_from`, after which the matcher judges the pair afresh: its next
deposit for the pair is dated no earlier than the undo. Facts stay on their
records, so every decision can be undone by another. The server resolves `a` and `b` to record IDs and logs those, so
the log names exactly the records decided on. Before logging, it answers 400
for what the store would refuse: rejecting an asserted link (split its
records instead), `a` and `b` naming one record, joining records of different
kinds, and joining or accepting a pair that a `distinct_from` between other
records of the two entities keeps apart (the message names that link; undo it
first). The decision is recorded with the caller as its actor, and its event
ID is the key that makes applying it once: accepting or rejecting a link that
is closed (or accepting one already asserted), and joining or splitting a pair
already that way, changes nothing and logs a warning. A decision the store
cannot carry out when it is applied (a link that has vanished since) becomes a
dead letter with the reason at once, without retries.

Only a person decides ([ADR 7](../adr/0007-records-identity-links-matchers.md):
a proposed link becomes asserted only by a person or a rule the settings
name), and the setting `api.link_decisions` says which callers count:
`socket` (the default) only callers on the Unix socket, `token` API token
holders too, `off` no one (C-API-8). The socket trusts every process running
as the service user (or root): the server refuses a peer it started itself,
such as a local adapter, or a child of one, but not a process that has left
the server's process tree, so `socket` is only as safe as the service
account (see the threat model's known gaps). An authenticator that knows its caller is
an agent sets `Caller.Agent`, and such a caller reads but always gets a 403 on
this route. Neither authenticator the server has can tell yet, so with
`token` a person who hands the token to an agent hands over the right to
decide too, and the decision is attributed to `token`. Telling an agent from a
person needs an identity provider that marks agents (OIDC); until then, keep
the default or hold the token as you would a password.

### Runtimes answer

`GET /v1/runtimes` answers one object. Lists of the mode in use are `[]` when
empty, never absent; fields of the other mode are absent.

| Field | Meaning |
| --- | --- |
| `language` | The language asked for, lower case, aliases resolved |
| `below_version` | The `below` parameter as parsed (only with `below`) |
| `entities` | Without `below`: every entity that declares the language |
| `below` | With `below`: entities with a declaration under it |
| `unknown` | With `below`: entities with none under it and one that cannot be read |
| `at_or_above` | With `below`: how many entities are in neither list (a number, 0 when none) |
| `truncated` | `true` when a list was cut at `limit` (absent otherwise) |

Each entity in a list is `{subject, declarations}`; `subject` is the same
reference other answers use. Each declaration has:

| Field | Meaning |
| --- | --- |
| `version` | The declared text, such as `>=18` |
| `file` | The manifest it came from |
| `lower` | The lowest version it allows (absent when `is_unknown`) |
| `is_below` | `lower` is under `below` (always `false` without `below`) |
| `is_unknown` | `version` cannot be read as a version or range; never guessed |
| `valid_from`, `valid_to` | When the fact held; `valid_to` is absent while it holds |
| `sources`, `source_count` | The observations behind it, as in every answer |

`below` must be digits and dots, such as `1.22` (`^[0-9]+(\.[0-9]+)*$`); a
range, operator, `v` prefix or wildcard (`>=1.22`, `v1.22`, `1.x`) is a 400. A
`!=` comparator in a declared range sets no lower bound and is skipped. A
declared version with a pre-release (`20.0.0-rc.1`, Go's `1.22rc1`) is
unknown, not its release; a `+build` suffix is ignored.

## Limits

Before any of the checks above, the TCP listener limits each remote peer (an
IPv6 peer is its /64): at most 64 open connections, 1,024 in all, closed as
they are accepted and before the TLS handshake, and 1,200 requests a minute
with a burst of 200 (else 429 `rate_limited`, counted whatever the token). It
speaks HTTP/1.1 only, so a connection carries one request at a time. The health
endpoints and the webhook route are not counted (the route has its own limit,
see [Webhook deliveries](#webhook-deliveries)), and the socket has no such
limit. Each caller has a request rate (default 600 a minute), a decision rate
on top of it (30 a minute, burst 3), at most two slow
reads in flight, and a deadline per request (30 seconds; 15 for a slow read).
The slow reads are `changes`, `find`, `links`, `runtimes` and `related` with `depth` above 1: they
scan or walk many rows, so they share one cap and the shorter deadline. A
caller is its token subject at a peer address (an IPv6 peer is its /64), or
`local:<uid>` on the socket, so one remote client cannot use up another's
share of a shared token. The server holds at most 256 requests at once, reads
headers for at most 10 seconds and 16 KiB, and refuses any body except the 8 KiB
of a decision.

## Settings

The listeners come from the `listen`, `auth` and `api` blocks of the settings file
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
| `api.link_decisions` | Who may decide identity links: `socket` (default), `token` or `off` |

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
| 403 | `forbidden` (a link decision from a caller `api.link_decisions` does not allow, or from an agent) |
| 404 | `not_found`, `unknown_source` |
| 405 | `method_not_allowed` |
| 413 | `too_large` |
| 415 | `unsupported_media_type` (a decision body that is not `application/json`) |
| 429 | `rate_limited`, `too_many_reads` |
| 500 | `internal` |
| 501 | `not_implemented` (the server takes no link decisions) |
| 503 | `busy`, `unavailable` (the event log refused a sync request or a decision) |
| 504 | `timeout` |

## The command line

`bearing get`, `owner`, `related`, `changes`, `find` and `runtimes` take `--server` (or
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

`bearing runtimes --language L [--below V] [--kind K] [--limit N] [--as-of T] [--json]`
answers "which services use language L below version V" from `--store` or
`--server`: one line per entity with its declared versions and files, those
below first, then the ones whose version cannot be read, then a count of the
rest.

`bearing source sync <source> [--server ADDR] [--json]` takes the same
`--server`, `--token-env` and `--server-ca` flags and `$BEARING_SERVER`. It
makes the sync request and prints the request's event ID and offset (or them as
JSON); the sync itself runs later.

`bearing links [--proposed] [--entity REF] [--limit N]` lists the open identity
links, strongest first (`--proposed` is the review queue), from `--store` or
`--server`, with the same answer from both, except that `--store` scores
links with the default half-lives (30 days) and does not read the server's
`matching.half_life`. `bearing link accept|reject|undo-distinct <link ID>`
and `bearing link join|split <ref> <ref>`, each with `--reason`, send a
decision and print the event ID and offset; they need `--server` (or
`$BEARING_SERVER`), since decisions go through the server's event log, and
`--store` is a usage error. The decision is applied a moment later, so
`bearing links` straight after may not show it yet.

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
