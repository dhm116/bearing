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

A spike measured SurrealDB, PostgreSQL and ClickHouse on those operations.
Its report and code are on the `spike/storage-engines` branch
([report](https://github.com/dhm116/bearing/blob/spike/storage-engines/docs/spikes/storage-engines.md)).
In short:

- Bearing's own 50,000-item conformance cases take 18 to 36 s on SurrealDB
  and 0.4 to 1.7 s on the engine alone, so 95% or more of the time is
  SurrealDB storing and loading rows.
- On a table shaped like surrealstore's, SurrealDB is 12 to 18 times slower
  than PostgreSQL at writes and over 100 times slower at point reads.
- SurrealDB's planner makes surrealstore's `version` load (`rec <= $head
  AND tbl = $t AND key IN $k`) scan most of the table, so loads slow down
  linearly with history. Two keys take 6.9 s on a 1M-row table, where
  PostgreSQL takes 3 ms.
- The cost of one SurrealDB transaction grows faster than its row count, so
  surrealstore stages a large apply in 2,000-row transactions that readers
  must skip until the head moves, and cleans up after attempts that never
  commit. That protocol exists only because of SurrealDB.

Options considered:

| Option | Pros | Cons |
| --- | --- | --- |
| Keep SurrealDB and fix its queries | No new backend | Slowest at every operation the store performs; planner problems found one query at a time; core is BSL 1.1; embedded bindings at v0.1.0; few operators know it |
| PostgreSQL with pgvector | Fastest at point loads and transactions, fast bulk loads, one apply in one transaction; pgvector HNSW for vectors in the same database; PostgreSQL License; known by every operator and offered by every cloud | A server to run (embedded options are below); a new backend to write |
| ClickHouse | Fastest bulk inserts and scans | Point loads 40 to 50 times slower than PostgreSQL; no production multi-statement transactions, so the head would need a second store; its scans go unused because the Go engine evaluates every rule |
| PostgreSQL for current state, ClickHouse for history | Columnar history | Every read takes a record time, so history is the state: an `AsOf` in the past and a backup would span two stores |

## Decision

- **PostgreSQL (16 or later) with pgvector is the default backend** for both
  contracts, one database for both as ADR 5 intended. `pkg/store` opens it
  from `postgres://` and `postgresql://` URLs, and pgvector 0.5 or later
  provides HNSW.
- **`pkg/store` keeps the store's settings in Bearing's hands**, because pgx
  reads more than the URL's user info. `BEARING_STORE_PASSWORD` is the only
  source of the password (C-STORE-1): the store refuses a password in the
  user info or in the `password`, `passfile` or `service` query parameters,
  and does not read `PG*` environment variables or password files. It
  accepts a named list of query parameters and refuses the rest. The
  SurrealDB-only `ns`, `db` and `auth` parameters go; a URL's path names
  the database. pgx defaults to `sslmode=prefer`, which falls back to
  plaintext without checking certificates, so the store requires
  `verify-full` unless the host is loopback or a Unix socket, or
  `insecure_store_plaintext` is set (C-STORE-6).
- **`internal/pgstore` is built the ADR 13 way.** It owns the row layout,
  migrations, transactions and backup plumbing, and runs every operation on
  the memstore engine. An apply is one transaction: it takes the head row
  with `SELECT ... FOR UPDATE`, returns `ErrStale` if `base_recorded_at`
  is not the head, then loads, decides and writes before it commits. This
  is the contract's compare-and-swap on the head, done under the row lock.
  A concurrent apply waits on the row instead of reloading after a lost
  race, which costs nothing because the single apply clock serializes
  applies anyway. Readers are not blocked, and there is no staging
  protocol.
  Reads run in `REPEATABLE READ READ ONLY` transactions, so each read sees
  one snapshot at one head.
- **`pgstore` keeps a canonical-subject table**, written in the same
  transaction as each merge and un-merge and versioned by record time
  (`rec` and `ret`, like the other rows), so a read at a past record time
  canonicalizes as the subjects stood then. An operation loads only the
  merge components of the subjects it names, which meets C-STORE-9's rule
  for merges ([#81](https://github.com/dhm116/bearing/issues/81), option 2).
  An operation still loads the whole history of each series it touches,
  and an unfiltered read still loads every series of its table, as in
  surrealstore. That part of the C-STORE-9 exception stays until a series
  can be loaded as of one record time. `DefaultMaxMerges` and
  `ErrTooManyMerges` go with surrealstore.
  *Amended 2026-10-09, when `pgstore` was built:* the table (`component`)
  maps each subject that any merge record ever joined to the lowest subject
  ID of its component and is not versioned by record time. It only decides
  which merge records to load, and the merge records themselves carry the
  record times that the engine canonicalizes by, so a read at a past record
  time gets the same answer. An un-merge ends a merge record and never
  splits a component, so the table never needs to shrink: components
  only grow, and a lookup that returns a larger component than a past
  record time had loads more records than needed, never fewer. Joining two
  components relabels the one with the higher label, in the apply's transaction.
- **The driver is pgx (MIT).**
- **`internal/surrealstore` is removed** in the change that makes
  `pgstore` the default, once `pgstore` passes both conformance suites in
  CI. With it go the `surrealembed` build tag, the `surrealdb+*` and
  `surrealkv://` URLs, the SurrealDB make targets and both SurrealDB Go
  modules. Bearing has no installs yet, so nothing needs migrating.
- **`mem://` stays** for tests, demos and trials with nothing to run.
- **Embedded PostgreSQL is the intended single-binary option, not built
  now.** [PGlite](https://github.com/electric-sql/pglite) is PostgreSQL
  compiled to WebAssembly, with pgvector, under Apache-2.0 and the
  PostgreSQL License. Run under wazero, the runtime ADR 9 chose for
  adapters, it would let `pgstore` serve a local install in process
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

- **Faster applies and reads.** Projected from the spike, not yet
  measured on `pgstore`: a 50,000-item apply should take about as long as
  the engine needs (a second or two) rather than 18 to 36 s, and point
  loads well under a millisecond. The ceiling of the single apply clock,
  which [#27](https://github.com/dhm116/bearing/issues/27) will now
  measure against PostgreSQL, rises accordingly, which may postpone the
  hybrid-clock relaxation.
- **Licences.** The default binary already links no BSL code; now no
  supported configuration runs BSL code at all. pgx is MIT and pgvector uses
  the PostgreSQL License; both are Apache-2.0 compatible. pgx is a new
  dependency, justified in the commit that adds it.
- **Operations.** Backups use `GraphStore.Backup` as today, plus whatever
  the operator already uses for PostgreSQL. High availability is PostgreSQL's
  own replication or a managed service.
- **Vectors.** pgvector fixes the dimension in the column type
  (`vector(n)`), where SurrealDB took it from the first vector written. So
  the dimension is configuration, read when the store opens, and the
  migration that creates the vector table uses it. Changing it means
  re-indexing from the graph, as before. HNSW indexes `vector` columns of
  up to 2,000 dimensions; a larger model needs `halfvec`.
- **The threat model's store section is rewritten for PostgreSQL** in the
  change that adds `pgstore`. Boundary B6 and T-STORE-1 name PostgreSQL.
  Each control changes as follows:
  - C-STORE-1: the password rules in the Decision.
  - C-STORE-2: Bearing connects as a role that owns only Bearing's schema;
    it is never a superuser and cannot create extensions.
  - C-STORE-3: compose generates the PostgreSQL password and does not
    publish its port.
  - C-STORE-4, which covers SurrealDB's network functions and scripting,
    is retired.
  - C-STORE-5: values reach SQL only as bound parameters. Role, schema and
    database names cannot be parameters in `CREATE ROLE` or `CREATE
    SCHEMA`, so they must match the same name pattern before they are
    quoted.
  - C-STORE-6: the TLS rules in the Decision.
  - C-STORE-9: the merge part of the exception closes; the series-history
    part stays (see the Decision).
  - C-STORE-10 names `pgstore`.

  pgvector is the only extension, and an administrator installs it.
  Migrations check that it exists and fail with a clear error if it
  doesn't; they never create it.
- **CI** runs the conformance suites against a PostgreSQL service in place
  of SurrealDB, within the five-minute budget.
- **Docs follow the code.** These change in the switch-over PR, not
  before:
  - `AGENTS.md`: architecture rule 5, the layout table and the SurrealDB
    test notes.
  - The README's "One database to start" section.
  - `docs/spec/contracts.md`.
  - `docs/spec/data-model.md`, whose apply-clock notes name SurrealDB.
  - `.github/reviewers.yml`, the PR template's SurrealDB checkbox and
    `.github/actions/surrealdb`.
  - The `db.system.name` attribute in `docs/telemetry.md`, which becomes
    `postgresql`.
  - ADR 2's note that one SurrealDB database backs both contracts, which
    gets a pointer here.
- **Issues.**
  - #81's work becomes part of `pgstore`, and its SurrealDB measurements
    are not needed.
  - These close with the backend: #99 (embedded SurrealDB untested), #97
    (unbounded commit transaction) and #75 (shared URL redaction, since
    only `pkg/store` will redact).
  - These carry over to `pgstore` in substance:
    - #94: refuse or warn on a superuser connection.
    - #95: a retryable give-up error, and checked row counts on updates.
    - #96: journal contiguity in Backup and Restore, and surviving a crash
      mid-Restore.
    - #98: TLS enforcement, now the `sslmode` rule above.
  - The resolver-state growth that
    [#77](https://github.com/dhm116/bearing/issues/77) planned for M3 is a
    data-model problem; the store choice doesn't change it.
- **Supersedes in part ADR 5**: its choice of SurrealDB, the embedded
  SurrealDB mode and its licence notes. Its separate contracts, URL-based
  `pkg/store` and one-database default stand. ADR 13 is unchanged; its
  description of how surrealstore works becomes history when the backend
  is removed.
