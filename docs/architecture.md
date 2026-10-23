# How Bearing works inside

A tour of the running server for someone new to the code. Each section has a
diagram and the packages to read next. The decisions behind it are in
[`docs/adr/`](adr/); the words (observation, record, fact, link) are in
[concepts](spec/concepts.md).

## The big picture

```mermaid
flowchart LR
  subgraph sources[Tools Bearing reads]
    GH[GitHub]
    K8s[Kubernetes]
    IdP[Directory]
  end
  subgraph adapters[Adapters, one process each]
    AGH[github adapter]
    AK8s[kubernetes adapter]
  end
  subgraph server[bearing server]
    API[API: socket and TCP]
    WH[Webhook route]
    LOG[(Event log)]
    W[Apply workers]
    M[Matcher]
    S[(Store: DuckDB)]
  end
  GH -- webhooks --> WH
  AGH -- reads --> GH
  AK8s -- reads --> K8s
  WH --> LOG
  W <-- gRPC: Sync, Handle --> AGH
  W <-- gRPC --> AK8s
  LOG --> W
  W -- Ingest --> S
  W -. sync done .-> M
  M -- links --> S
  CLI[bearing CLI / MCP] --> API --> S
```

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

```mermaid
sequenceDiagram
  participant Sch as Scheduler or API
  participant Log as Event log
  participant Wk as Apply worker
  participant Ad as Adapter
  participant St as Store
  participant Mt as Matcher
  Sch->>Log: SyncRequested
  Log->>Wk: next event for this source
  loop each page
    Wk->>Ad: Sync(config, cursor)
    Ad-->>Wk: observations, next cursor, done?
    Wk->>Log: ObservationsEmitted (one page)
  end
  Log->>Wk: each page in turn
  Wk->>St: Ingest(page, authority, full-sync kinds)
  Note over Wk,St: the last page carries sync_started, so records<br/>the source no longer lists have their facts closed
  Wk--)Mt: sync done (a signal, not a wait)
  Mt->>St: propose or assert identity links
```

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

```mermaid
sequenceDiagram
  participant Src as GitHub
  participant Rt as Webhook route
  participant Log as Event log
  participant Wk as Apply worker
  participant Ad as Adapter
  participant St as Store
  Src->>Rt: POST /webhooks/{source}
  Note over Rt: 1. per-peer rate limit<br/>2. body size limit<br/>3. HMAC signature, constant time<br/>4. per-source rate limit
  Rt->>Log: WebhookReceived (body and headers)
  Rt-->>Src: 202 Accepted, only once it is logged
  Log->>Wk: next event for this source
  Wk->>Ad: Handle(config, headers, body)
  Ad-->>Wk: observations
  Wk->>Log: ObservationsEmitted
  Log->>Wk: the page
  Wk->>St: Ingest
```

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

```mermaid
flowchart TD
  O[Observation] --> D{Seen this ID before?}
  D -- yes --> X[Skip it]
  D -- no --> L[Log it in observation]
  L --> R[Find or create the record<br/>for its key and aliases]
  R --> I{Linked IDs?}
  I -- source vouches --> A[Asserted same_as link]
  I -- source doesn't --> P[Proposed same_as link]
  R --> F[Open, refresh or close facts:<br/>relations, attributes, qualifiers]
  A --> C[Recompute clusters:<br/>records joined by asserted links]
  F --> C
```

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

```mermaid
sequenceDiagram
  participant U as bearing CLI or a client
  participant API as API
  participant Q as pkg/query
  participant St as Store
  U->>API: GET /v1/owner?ref=github:repo/acme/payments
  Note over API: socket: peer UID check<br/>TCP: bearer token, TLS, limits
  API->>Q: Owners(ref)
  Q->>St: Entity, Facts (asserted only)
  St-->>Q: facts with their sources
  Q-->>API: one fact per statement, all sources
  API-->>U: JSON answer with evidence
```

Read next: `pkg/server/api.go`, `pkg/query`, [the API spec](spec/api.md).
