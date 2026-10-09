# 14. PostgreSQL is the default store, and the SurrealDB backend goes

Date: 2026-10-09 · Status: proposed

## Context

[ADR 5](0005-one-store-to-start.md) made SurrealDB the default for both
`GraphStore` and `VectorIndex`, for one dependency and a native graph model.
[ADR 13](0013-backends-store-rows-one-engine-applies-rules.md) then moved
every data-model rule into one Go engine (`internal/memstore`). A backend now
stores rows: `internal/surrealstore` keeps protobuf blobs in generic `series`
and `version` tables and uses none of SurrealDB's graph features. What a
backend has to be good at follows from that:

- **Indexed point loads.** Every apply and every read loads the series it
  names by subject, key or predicate into a scratch engine.
- **Bulk writes.** A `ChangeSet` can carry 50,000 items in each list
  (`pkg/contracts/limits.go`).
- **Real transactions.** An apply checks and moves the head, marks the
  event processed and writes its rows atomically.

A spike measured SurrealDB, PostgreSQL and ClickHouse on those operations
([report](../spikes/storage-engines.md)). In short:

- Bearing's own 50,000-item conformance applies take 18 to 36 s on SurrealDB
  and 0.4 to 1.7 s in the engine alone, so 95% or more of an apply is
  SurrealDB storing rows.
- On a table shaped like surrealstore's, SurrealDB is 6 to 20 times slower
  than PostgreSQL at writes and over 100 times slower at point reads.
- SurrealDB's planner turns surrealstore's load query (`key IN $k` over a
  composite index) into a scan of the whole table prefix, so reads slow down
  linearly with history. Two keys take seconds on a 1M-row table, where
  PostgreSQL takes milliseconds.
- The cost of one SurrealDB transaction grows faster than its row count, so
  surrealstore stages a large apply in 2,000-row transactions that readers
  must skip until the head moves, and cleans up after attempts that never
  commit. That protocol exists only because of SurrealDB.

Options considered:

| Option | Pros | Cons |
| --- | --- | --- |
| Keep SurrealDB and fix its queries | No new backend | Slowest at every operation the store performs; planner problems found one query at a time; core is BSL 1.1; embedded bindings at v0.1.0; few operators know it |
| PostgreSQL with pgvector | Fastest at point loads and transactions, fast bulk loads, one apply in one transaction; pgvector HNSW for vectors in the same database; PostgreSQL License; known by every operator and offered by every cloud | A server to run (embedded options are below); a new backend to write |
| ClickHouse | Fastest bulk inserts and scans | Point loads 35 to 50 times slower than PostgreSQL; no production multi-statement transactions, so the head would need a second store; its scans go unused because the Go engine evaluates every rule |
| PostgreSQL for current state, ClickHouse for history | Columnar history | Every read takes a record time, so history is the state: an `AsOf` in the past and a backup would span two stores |

## Decision

- **PostgreSQL (16 or later) with pgvector is the default backend** for both
  contracts, one database for both as ADR 5 intended. `pkg/store` opens it
  from `postgres://` and `postgresql://` URLs. The password comes from
  `BEARING_STORE_PASSWORD` as today; a URL that contains one is still
  rejected (C-STORE-1).
- **`internal/pgstore` is built the ADR 13 way.** It owns the row layout,
  migrations, transactions and backup plumbing, and runs every operation on
  the memstore engine. An apply is one transaction that locks the head row,
  so there is no staging protocol. It keeps a canonical-subject table
  written in the same transaction as each merge and un-merge, so an
  operation loads only the merge components of the subjects it names
  ([#81](https://github.com/dhm116/bearing/issues/81), option 2). The
  driver is pgx (MIT).
- **`internal/surrealstore` is removed** in the change that makes
  `pgstore` the default, once `pgstore` passes both conformance suites in
  CI. With it go the `surrealembed` build tag, the `surrealdb+*` and
  `surrealkv://` URLs, the SurrealDB make targets and both SurrealDB Go
  modules. Bearing has no installs yet, so nothing needs migrating.
- **`mem://` stays** for tests, demos and trials with nothing to run.
- **Embedded PostgreSQL is the intended single-binary option, not built
  now.** [PGlite](https://github.com/electric-sql/pglite) is PostgreSQL
  compiled to WebAssembly, with pgvector, under Apache-2.0 and the
  PostgreSQL License. Run under wazero, which Bearing already uses for
  adapters (ADR 9), it would let `pgstore` serve a local install in process
  with the same SQL, migrations and extensions, and without CGO. Its
  supported runtimes are JavaScript ones today. The Go drivers for its WASI
  build are pre-alpha and do not show pgvector working, and one engine
  serves one connection. A spike decides it when a single-binary install is
  needed. Until then, local installs run PostgreSQL from compose.
- **ClickHouse is not a `GraphStore` backend.** If installs grow to need
  analytics over the event log, the audit log or fact history, it can serve
  a derived projection behind a contract of its own, fed from the change
  journal and rebuildable from the graph as the vector index is
  ([ADR 2](0002-graph-is-source-of-truth.md)). That needs its own ADR.

## Consequences

- **Faster applies and reads.** A 50,000-item apply is expected to take
  about as long as the engine needs (a second or two) rather than 20 to 40 s,
  and point loads take well under a millisecond. The ceiling of the single
  apply clock measured by [#27](https://github.com/dhm116/bearing/issues/27)
  rises accordingly, which may postpone the hybrid-clock relaxation.
- **Licences.** The default binary already links no BSL code; now no
  supported configuration runs BSL code at all. pgx is MIT and pgvector uses
  the PostgreSQL License; both are Apache-2.0 compatible. pgx is a new
  dependency, justified in the commit that adds it.
- **Operations.** Backups use `GraphStore.Backup` as today, plus whatever
  the operator already uses for PostgreSQL. High availability is PostgreSQL's
  own replication or a managed service. The pgvector index's dimension is
  fixed by its column, as the HNSW index's was.
- **The threat model's store section is rewritten for PostgreSQL** in the
  change that adds `pgstore`: a database-scoped role that owns only Bearing's
  schema (C-STORE-2), TLS with `sslmode=verify-full` outside local
  development (C-STORE-6), no superuser, and no extensions beyond pgvector.
  The SurrealDB-specific controls (network functions and scripting,
  C-STORE-4) are retired with the backend.
- **CI** runs the conformance suites against a PostgreSQL service in place
  of SurrealDB, within the five-minute budget.
- **Docs follow the code.** `AGENTS.md` (architecture rule 5, the layout
  table, the SurrealDB test notes), the README's "One database to start"
  section and `docs/spec/contracts.md` change in the switch-over PR, not
  before.
- **Issues.** #81's work becomes part of `pgstore`, and its SurrealDB
  measurements are not needed. #99 (embedded SurrealDB untested) closes
  with the backend. [#77](https://github.com/dhm116/bearing/issues/77)'s
  state growth is a data-model problem and is unchanged.
- **Supersedes in part ADR 5**: its choice of SurrealDB, the embedded
  SurrealDB mode and its licence notes. Its separate contracts, URL-based
  `pkg/store` and one-database default stand. ADR 13 is unchanged; its
  description of how surrealstore works becomes history when the backend
  is removed.
