# Component contracts

Bearing's core is a set of components that talk only through the interfaces
below. Each has one default backend; any other backend that passes the
interface's conformance suite can replace it. The Go definitions are in
[`pkg/contracts`](../../pkg/contracts/contracts.go).

| Interface | Responsibility | Default | Alternatives | Conformance suite |
| --- | --- | --- | --- | --- |
| `GraphStore` | Subjects, alias bindings, merges and the resolver's state, bitemporally; supports, facts, conflicts and data-quality issues to follow ([below](#graphstore)). The source of truth. | SurrealDB (from issue #44; in-memory until then) | PostgreSQL, Neo4j, Apache AGE, Memgraph | Yes (`conformance.GraphStore`) |
| `VectorIndex` | Semantic search over subjects and documents, keyed by subject ID | SurrealDB | Qdrant, pgvector, OpenSearch, Weaviate | Yes (`conformance.VectorIndex`) |
| `EventBus` | At-least-once delivery of CloudEvents between components | NATS JetStream | Kafka, SQS/SNS, Postgres queue | Planned |
| `Extractor` | Proposes candidate entities and relations from unstructured text | Any chat-completions style LLM API | Hosted or local models | Planned |
| `Judge` | Calibrated typed judgments: choice, yes/no, score | Kev 4B, self-hosted | Jev hosted API | Planned |
| `PolicyDecider` | Allow or deny an action, with the reason and how to fix it | Open Policy Agent | Cedar | Planned |
| `Executor` | Plan, apply, verify and roll back actions durably | Temporal | Postgres-backed job runner | Planned |

## One database to start

A backend can serve more than one interface. By default SurrealDB backs
both `GraphStore` and `VectorIndex`, so a small install runs one database, or
none with an embedded build ([ADR 5](../adr/0005-one-store-to-start.md)).
[`pkg/store`](../../pkg/store) opens backends from URLs:

| URL | Backend | Needs |
| --- | --- | --- |
| `mem://` | In-memory reference store | Nothing; data is lost on exit |
| `surrealdb+ws://user@host:8000?ns=bearing&db=main` | SurrealDB server (also `wss`, `http`, `https`) | A running `surreal start` |
| `surrealdb+mem://` | Embedded SurrealDB in memory | A `-tags surrealembed` build (CGO) |
| `surrealkv:///var/lib/bearing` | Embedded SurrealDB on disk | A `-tags surrealembed` build (CGO) |

`store.Config{Graph: url}` uses one backend for both. Setting
`Config.Vectors` to a second URL splits them, for example a SurrealDB graph
with Qdrant vectors, and nothing else changes. Passwords come from
`BEARING_STORE_PASSWORD`, not the URL; opening a URL that carries a password
MUST fail.

In a vector index, a point's kind is its `kind` payload field, which
`VectorQuery.Kinds` filters on. When subjects merge, the core calls
`Repoint` to move the merged subject's points to the survivor.

Until issue #44, the SurrealDB backend serves only `VectorIndex`: opening a
SurrealDB URL as the graph fails with "not implemented until issue #44".
Use `mem://` for the graph meanwhile.

Beyond Go interfaces, components that run as separate services expose the
same operations over a network protocol (gRPC or HTTP+JSON), so a backend can
be written in any language. Those wire definitions will live in `proto/`.

## GraphStore

`GraphStore` holds the state of the [data model](data-model.md): the
identity store (subjects, alias bindings, merge and un-merge records) and,
in later changes for issue #37, the claim store (supports, fact statuses,
conflicts, data-quality issues). The work is split in two:

- The **resolver** (issue #43) reads the store, applies the data model's
  rules to one event, and writes the outcome as a `ChangeSet`.
- The **store** applies a `ChangeSet` atomically and once per event ID,
  records it with a strictly increasing record time, and answers
  bitemporal reads. It checks only what it can check cheaply and without
  configuration.

### ChangeSet

A `ChangeSet` ([`changeset.proto`](../../proto/bearing/model/v1alpha1/changeset.proto))
is one event's writes:

| Field | Meaning |
| --- | --- |
| `event_id` | The event applied. The store applies each event ID once; a repeat writes nothing and returns the original result with `Duplicate` set. |
| `base_recorded_at` | The head (latest `recorded_at`) the resolver read at. If another apply has landed since, `Apply` fails with `ErrStale` and the resolver recomputes. |
| `recorded_at` | Set by the store. |
| `mints` | Subjects to mint, each with a ref `new:<label>` and a kind. |
| `bindings` | Alias binding timelines. |
| `merges` | Applied in order, each seeing the merges before it. Each needs a rule. |
| `unmerges` | Each un-merge's target is resolved against the state before the `ChangeSet`, not after the un-merges listed before it, and two un-merges may not claim the same merge record or subject. An un-merge has a ref (`new:<label>`, required, unique among the `ChangeSet`'s mints and un-merges) for the subject its aliases move to. |
| `state` | The resolver's own entries (ordering keys, watermarks, sync progress), as `google.protobuf.Any`. |

Anywhere a subject ID appears in a `ChangeSet`, a ref may stand for a
subject the same `ChangeSet` creates; the store substitutes the minted ID.
A timeline item replaces its series' current timeline: rows equal to a
current row keep their `recorded_at`, other current rows are retracted at
this apply's record time, new rows are recorded at it, and an empty
timeline retracts the series. An alias or state key appears at most once
in a `ChangeSet`. A support's `last_confirmed_at` is the exception to
versioning: a confirmation updates it in place, without a new version, so
it is not bitemporal. Items apply in this order: mints, un-merge targets,
bindings, merges, un-merges, state.

A `ChangeSet` larger than `contracts.MaxChangeSetBytes` (4 MiB, by
`proto.Size`) is refused, as is one that grows past it when recorded, so
every backup a store writes can be restored. A failed `Apply` writes
nothing and returns a zero result. A repeated event ID returns the original
apply's result with `Duplicate` set, so a redelivery after a crash can
still re-point vectors.

The store keeps every applied `ChangeSet`, with the IDs it minted and what
it decided, in its change journal (not the audit log,
[ADR 8](../adr/0008-audit-log.md)).

Subject IDs are UUIDv7s with timestamps no later than the record time of
the apply that minted them. After a restore the store seeds its ID source
with the last restored ID, so if the clock is behind the restored data, new
IDs carry that ID's timestamp: IDs stay strictly increasing and can drift
from wall-clock time.

### Backup

`Backup` writes a stream of length-delimited `BackupFrame` messages
([`backup.proto`](../../proto/bearing/model/v1alpha1/backup.proto)), read
and written with `contracts.BackupReader` and `contracts.BackupWriter`:

1. A header: the body's format name and version, the store's head and the
   last subject ID it minted.
2. The body: records in the format the header names, which the store
   defines. No record frame is larger than `MaxChangeSetBytes`.
3. A trailer: the number of records and a SHA-256 of every byte before it.

`Restore` works only into an empty store. It MUST refuse a format or
version it doesn't know, a stream without its trailer (one truncated, even at
a frame boundary), a bad checksum or record count, and data after the
trailer, and MUST leave the store empty on any failure. After a restore,
every read at every record time up to the head answers as it did in the
original store, processed events stay processed, and the head and the order
of subject IDs are kept.

The reference store's body is its change journal, one `JournalEntry` per
record: each `ChangeSet` as applied, with the merge records, un-merge
targets (reactivated or split) and mints it decided. Restore replays the
journal and fails if any apply decides differently, so a change of rules
can't silently re-decide history. It also refuses an entry with no
`recorded_at` or one later than the clock plus five minutes.

### Who owns which rule

References are to sections of the [data model](data-model.md).

| Rule | Owner |
| --- | --- |
| Apply and isolation: one transaction per event, the processed-event mark, `recorded_at = max(now, previous + 1 µs)`, serialized applies | Store. Applies are serialized by compare-and-swap on the head (`ErrStale`). |
| Idempotency: re-applying a processed event is a no-op | Store |
| Subjects: minting UUIDv7 IDs in increasing order, never reused, also after a restore | Store |
| Minting: when to mint and with which rule | Resolver |
| Bindings: what each binding write maps, `per_subject`, back-extension, tentative bindings, redirects | Resolver, which writes the resulting timeline |
| Resolution: looking up an alias at a valid and record time, following merges | Store (`ResolveKey`, `Bindings`) |
| Resolution rules 1–3, rejections such as `kind_mismatch` | Resolver |
| Merge: survivor is the lower ID, `status`/`merged_into`, reads canonicalize from `r`, earlier reads show two subjects, alias sets on the record. An alias set holds every alias with a row mapping it to the subject as recorded at the merge, released rows that redirect and tentative rows included. | Store |
| Merge: same kind, both active | Store checks; resolver decides |
| Merge triggers, policies, the guard, evidence re-evaluation | Resolver |
| Un-merge: reactivating the subject whose alias set matches, else a `split` mint; no un-merge of a `placeholder` merge; aliases a non-empty proper subset | Store |
| Un-merge: re-pointing bindings and claims, setting `distinct_from` | Resolver. It re-points by the binding rows as written, not by alias membership. |
| Backup and restore of primary state (ADR 11) | Store ([Backup](#backup)) |
| Vector points re-pointed on merge | Core, through `VectorIndex.Repoint` |
| Claims, supports, ordering and idempotency of claims, snapshot scopes, sync completeness, derived claims, confidence, status, conflicts, matching, manual operations, audit | Resolver; the store's part arrives with the claim-store reads |

### Reads

Every read takes a record time and returns what was recorded at or before
it; `ResolveKey` also takes a valid time. A zero time means now. Subject
IDs in answers are canonical as of the record time, except where a method
returns rows as written (`Bindings`). The resolver reads the head with
`Head`, reads at that record time, and sets `base_recorded_at` to it, so
its reads are a consistent snapshot.

## Rules that apply to every component

- **The graph is the source of truth.** The vector index, caches and search
  are derived from it and can be rebuilt from it. Policy and ownership
  answers read only asserted graph facts.
- **Candidates are not facts.** Anything an `Extractor` proposes goes through
  a `Judge` before it reaches the graph.
- **Idempotent consumers.** Event delivery is at least once. Handlers must
  tolerate duplicates.
- **Least privilege.** Components that read external systems (adapters) hold
  read-only credentials. Only the `Executor` holds write credentials, and
  only after a `PolicyDecider` allows the action.

## Adding a backend

1. Implement the interface.
2. Call the conformance suite from your backend's tests:

   ```go
   func TestConformance(t *testing.T) {
       conformance.GraphStore(t, func(t *testing.T) (contracts.GraphStore, conformance.Clock, conformance.IDs) {
           clk := testkit.NewClock(time.Time{})
           ids := testkit.NewUUIDv7s(clk.Now)
           return newEmptyStoreForTest(t, clk.Now, ids), clk, ids
       })
   }
   ```

   The suite drives the store's clock, and reads the last ID its ID
   source issued; `testkit.FakeClock` satisfies `conformance.Clock` and
   `model.UUIDv7Source` satisfies `conformance.IDs`.

3. Document any behavior the suite doesn't cover (consistency, limits).

[`internal/memstore`](../../internal/memstore) is the reference `GraphStore`
and `VectorIndex` and shows the pattern.
[`internal/surrealstore`](../../internal/surrealstore) passes the
`VectorIndex` suite; its `GraphStore` arrives with issue #44.
