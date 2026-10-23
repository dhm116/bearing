# How Bearing works inside

A tour of the running server for someone new to the code. Each section has a
diagram and the packages to read next. The decisions behind it are in
[`docs/adr/`](adr/); the words (observation, record, fact, link) are in
[concepts](spec/concepts.md).

## The big picture

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/architecture-overview-dark.svg">
  <img src="diagrams/architecture-overview.svg" alt="Architecture of the Bearing server. Source systems (GitHub, Kubernetes, a directory) send webhooks to the webhook route, and adapters, one process each, read them. The route appends to the event log; apply workers take events from it in order, call the adapters over gRPC for observations and ingest them into the store (DuckDB). After each sync the workers signal the matcher, which writes links to the store. The CLI and MCP ask questions through the API, which reads the store.">
</picture>

- **Adapters** (`adapters/*`, `pkg/adapter`) only read a source and turn
  what they see into observations. The server starts each one as its own
  process and talks to it over gRPC on a private Unix socket, or reaches a
  remote one over TLS with a token.
- **The event log** (`pkg/store`, `internal/duckstore/eventlog.go`) is the
  server's to-do list. Everything that will change the graph is written there
  first, so nothing is lost if the server stops halfway.
- **Apply workers** (`pkg/server`) take events off the log in order, one
  worker per source, and do the work: call the adapter, then ingest what it
  returned.
- **The store** (`internal/duckstore`, behind `pkg/store` and `pkg/graph`)
  keeps observations, records, identity links and facts, and answers
  questions.
- **The matcher** (`pkg/match`) looks for records that describe the same
  thing after each sync and proposes links between them.

## A sync

A sync reads everything a source has, page by page. It starts on a schedule
or when someone runs `bearing source sync`.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/sync-dark.svg">
  <img src="diagrams/sync.svg" alt="Sequence of a sync. A SyncRequested event goes into the event log and the apply worker takes it. For each page it calls the adapter's Sync with a cursor, gets observations and the next cursor back and appends the page to the log. It then ingests each page into the store; only the last page carries sync_started, from the adapter's clock, so only then does the store close what the source stopped listing. Last, the worker signals the matcher without waiting, and the matcher writes identity links to the store.">
</picture>

- Pages are logged before they are ingested, so a crash replays them; the
  store skips an observation it already has.
- Only the **last page of a full sync** says "this is everything", and only
  then does the store close what the source stopped listing. Its time comes
  from the adapter's own clock (the earliest observation of the sync).
- Matching runs on its own goroutine, so a slow match never holds up the
  next sync.

Read next: `pkg/server/events.go` (`handleSync`, `handleObservations`),
`pkg/server/apply.go`, `internal/duckstore/ingest.go`.

## A webhook

A source can also push a change as it happens. The server checks the
delivery, logs it and answers at once; the adapter turns it into
observations later.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/webhook-dark.svg">
  <img src="diagrams/webhook.svg" alt="Sequence of a webhook. GitHub posts to /webhooks/{source}. The route checks, in order, a per-peer rate limit, the body size limit, the HMAC signature in constant time and a per-source rate limit, logs the delivery and only then answers 202 Accepted. Later the apply worker takes the event, has the adapter's Handle turn it into observations, appends that page to the log and ingests it into the store.">
</picture>

- **The server owns verification.** The route uses the signature scheme the
  adapter's declaration names and the secret from the server's settings. A
  bad or missing signature and an unknown source get the same `401`, so a
  stranger learns nothing.
- **The adapter never needs the webhook secret.** By the time it sees a
  delivery, the server has checked it and logged it. (Today the GitHub
  adapter still checks again, with a secret the server deliberately doesn't
  give it, so deliveries fail; removing that check is pending.)
- A webhook page is never a full sync, so it doesn't close anything and
  doesn't start matching.

Read next: `pkg/server/ingest.go` (`serve`), `pkg/server/verify.go`,
`pkg/server/events.go` (`handleWebhook`).

## Inside an ingest: records, links and facts

Each `Ingest` call is one transaction: everything in it lands, or nothing
does.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/ingest-dark.svg">
  <img src="diagrams/ingest.svg" alt="Flowchart of one ingest. An observation whose ID was seen before is skipped. Otherwise it is logged in the observation table and its record is found or created by key and aliases. Linked IDs become an asserted same_as link when the source vouches for them and a proposed link, which joins nothing yet, when it does not. Facts are opened, refreshed or closed, and clusters of records joined by asserted links are recomputed.">
</picture>

- A **record** is one thing as one source described it. An **entity** is a
  cluster of records joined by **asserted** `same_as` links; answers are
  about entities.
- A **proposed** link joins nothing until a person or a rule in the settings
  asserts it. Answers about ownership and dependencies read asserted facts
  only.
- Closing a link splits the cluster again, and every fact stays on the record
  that observed it, so a wrong join can be undone.

Read next: `internal/duckstore/ingest.go`, `links.go`, ADR 7.

## A question

The CLI, or any client, asks the API, for example
`GET /v1/owner?ref=github:repo/acme/payments`. The API checks the caller
(the peer's UID on the socket; a bearer token, TLS and rate limits on TCP),
and `pkg/query` reads only asserted facts from the store and answers in JSON
with one fact per statement and every source behind it.

Read next: `pkg/server/api.go`, `pkg/query`, [the API spec](spec/api.md).
