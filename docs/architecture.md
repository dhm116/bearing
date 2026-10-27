# How Bearing works inside

A tour of the running server for someone new to the code. Each section has a
diagram and the packages to read next. The decisions behind it are in
[`docs/adr/`](adr/); the words (observation, record, fact, link) are in
[concepts](spec/concepts.md).

## The big picture

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/server-flow-dark.svg">
  <img src="diagrams/server-flow.svg" alt="Architecture of the Bearing server with a sync traced through it: a schedule or bearing source sync asks the API, which logs a request in the event log; the source's apply worker takes it in order and calls the adapter's Sync over gRPC; the adapter reads the source system and returns a page, which is logged and then ingested into the DuckDB store, the last page closing what the source stopped listing; the worker then signals the matcher, which writes identity links. Webhooks take the same path after the webhook route checks and logs them, and the CLI and MCP read asserted facts through the API.">
</picture>

- **Adapters** (`adapters/*`, `pkg/adapter`) only read a source and turn
  what they see into observations. The server starts each one as its own
  process and talks to it over gRPC on a private Unix socket, or reaches a
  remote one over mutual TLS (each side checks the other's certificate).
- **The event log** (`pkg/store`, `internal/duckstore/eventlog.go`) is the
  server's to-do list. Everything that will change the graph is written there
  first, so nothing is lost if the server stops halfway.
- **Apply workers** (`pkg/server`) take events off the log in order, one
  worker per source, and do the work: call the adapter, then ingest what it
  returned.
- **The store** (`internal/duckstore`, behind `pkg/store` and `pkg/graph`)
  keeps observations, records, identity links and facts, and answers
  questions.
- The numbers trace one sync, 1 to 6; 7 is a webhook joining the same path
  and 8 a question.
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
  delivery, the server has checked it and logged it.
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
- A **proposed** link joins nothing until a rule in the settings asserts it
  or a person accepts it (`bearing link accept`; the decision goes through
  the event log). Answers about ownership and dependencies read asserted facts
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

## What is planned

The parts on the [roadmap](../ROADMAP.md) plug into the same flow. Dashed
boxes are not built yet.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/server-planned-dark.svg">
  <img src="diagrams/server-planned.svg" alt="Today's flow, adapters to the event log and workers to the DuckDB store to the API, with the matcher writing links, and the planned parts around it: inference reads READMEs and runbooks and sends proposed facts into ingest; search indexes the store for lookups through the API; automation acts on fact changes; a model matcher adds same-or-not judgements to the matcher. Also planned are an HTTP MCP endpoint, OIDC and GraphQL on the API, and attestation of provenance.">
</picture>

- **Inference** reads text (READMEs, runbooks) and sends **proposed** facts,
  with the quoted span as evidence, through ingest like any other page.
  Proposals never become answers on their own.
- **A model matcher** is one more matcher: its judgements feed the same
  proposals a person reviews.
- **Search** and **automation** read the store; neither changes how facts get
  in.

## Where new parts plug in

Bearing grows at two seams, so new sources and stores don't change the
server.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="diagrams/server-extension-dark.svg">
  <img src="diagrams/server-extension.svg" alt="The two seams where Bearing grows: the AdapterService gRPC contract (Describe, Sync, Handle), which today's GitHub, Authentik and Kubernetes adapters speak and later AWS, PagerDuty and Backstage adapters, sandboxed or remote, will speak too; and the store interface, which DuckDB implements today and Quack or DuckLake could implement for more than one writer. Clients ask the server through the API: the CLI, REST and MCP over stdio today, and an HTTP MCP endpoint, GraphQL and OIDC later.">
</picture>

- **A new source is a new adapter.** It speaks the `AdapterService` gRPC
  contract (`proto/bearing/adapter`), locally or remotely over mutual TLS.
- **A new store implements the store interface** (`pkg/store`,
  `pkg/graph`). DuckDB is the only one today ([ADR 8](adr/0008-ducklake-not-now.md)).
- **New clients ask the same API.**

Open [`diagrams/server-flow.html`](diagrams/server-flow.html) in a browser to
step through the numbered sync above one stage at a time.
