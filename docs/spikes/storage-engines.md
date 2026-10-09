# Spike: storage engines for the graph store

Date: 2026-10-09 · Decides: [ADR 14](../adr/0014-postgres-is-the-default-store.md)
· Builds on: [ADR 13](../adr/0013-backends-store-rows-one-engine-applies-rules.md), #54, #81

**Question.** Since ADR 13, a backend stores rows and the memstore engine
decides every rule. Which engine does that job best at real sizes:
SurrealDB (the ADR 5 default), PostgreSQL or ClickHouse?

**Answer.** PostgreSQL. It leads on the operations every apply and read
performs: indexed point loads (over 100 times faster than SurrealDB, 40 to
50 times faster than ClickHouse) and atomic bulk writes (12 to 18 times
faster than SurrealDB). ClickHouse wins scans and raw inserts, but the
engine never asks the store for a scan, and ClickHouse has no production
multi-statement transactions. SurrealDB's planner also turns surrealstore's
`version` load query into a scan of most of the table.

## Setup

Code is in [`spikes/storage-engines/`](../../spikes/storage-engines/):
`gen.py` writes the rows, `postgres.sh` and `clickhouse.sh` drive those
engines with their own clients, and `surrealdb/` is a small Go module that
drives SurrealDB over HTTP. The root module is untouched.

- **Rows.** A table shaped like surrealstore's `version` table: `tbl`, `key`,
  `n`, `rec` and `ret` (record times in µs) and about 300 bytes of `data`,
  indexed on `(tbl, key)` and `rec` as surrealstore indexes it. The seed is
  1M rows, 200,000 series of 5 versions each. An apply adds 50,000 new rows
  at one record time.
- **Engines.** PostgreSQL 16 (Debian package), ClickHouse 25.8.10.7 LTS
  (MergeTree ordered by `(tbl, key, n)`), SurrealDB 3.3.0 (`surrealkv` on
  disk). All three on default settings, on disk, one at a time, on one
  4-core, 15 GB container.
- **SurrealDB writes** use surrealstore's chunking: 500 rows per `INSERT`
  and 2,000 per transaction (`chunkRows`, `stageRows` in
  `internal/surrealstore/apply.go`).

## Results

| Operation | PostgreSQL 16 | ClickHouse 25.8 | SurrealDB 3.3 |
| --- | --- | --- | --- |
| Seed 1M rows | 9.4 s | 5.2 s | 166 s |
| Apply 50,000 new rows atomically | 0.53 s, one transaction | 0.29 s, one insert | 9.4 s, 25 staged transactions |
| Supersede 5,000 rows (set `ret`) | 0.26 s | 0.10 s lightweight update, 0.16 s mutation | 3.1 s |
| Point read of one series as of a time, 1 client | 0.17 ms, 6,000/s | about 6.6 ms, 150/s | 34 ms p50, 28/s |
| The same, 16 clients | 0.52 ms, 31,000/s | 630/s | 138 ms p50, 112/s |
| Load two series as surrealstore's `version` load does (`rec <= head`, `key IN` a list) | 3.0 ms | 10 ms | 6.9 s with the keys bound as `$k`; 0.78 s as `OR`ed equalities |
| Scan: versions per table in the first half of the seed's record times | 105 ms | 16 to 21 ms | 10.1 s |
| Size on disk | 443 MB | 315 MB | not measured |

Bearing's own conformance cases tell the same story end to end. Each
count-limit case below builds a ChangeSet at the limit, checks that one item
more is refused, applies it and reads it back. Run serially against the same
SurrealDB server, and against the engine alone (`internal/memstore`):

| 50,000-item case | Engine alone | SurrealDB backend |
| --- | --- | --- |
| state entries | 0.4 s | 17.9 s |
| binding timelines | 0.8 s | 20.3 s |
| fact timelines | 1.7 s | 36.4 s |

## Findings

1. **SurrealDB does the store's work slowly.** The engine takes 2 to 5% of
   a large case's time; the rest is SurrealDB reading and writing rows. Its transactions also
   cost more than their row count (surrealstore's comments record 8,000
   rows taking 10 s in one transaction and 40,000 taking minutes), which is
   why surrealstore stages large applies.
2. **SurrealDB's planner makes surrealstore's `version` load scan.** The
   query is `rec <= $head AND tbl = $t AND key IN $k`
   (`internal/surrealstore/load.go`). Its plan (printed by the spike) is a
   `BitmapAnd` of a scan of the `(tbl, key)` index and a scan of the `rec`
   index. With `$k` bound as a list, the first reads the whole `tbl`
   prefix: 6.9 s for two keys. With the keys written as `OR`ed equalities
   it finds them directly, but the `rec <= $head` range still matches
   nearly every row: 0.78 s. Either way the cost grows with total history,
   not with the rows loaded. The `series`, `series_subject` and `subject`
   loads use the same `rec <= $head` filter with `IN` lists over their own
   indexes; the spike did not measure them. The conformance suite cannot
   see any of this, because its stores are small.
3. **ClickHouse is built for a different job.** It answers point reads from
   8,192-row granules and limits concurrent queries by design, so a point
   load costs milliseconds where PostgreSQL's B-tree costs a fraction of
   one. Updates work but are applied as part merges. It has no production
   multi-statement transactions, so an apply's head check, processed-event
   mark and rows could not commit together without a second store. Its
   scan speed only pays off if the rules move into SQL, which ADR 13
   rejected.
4. **PostgreSQL fits ADR 13 directly.** A 50,000-row apply fits in one
   transaction in under half a second, so `pgstore` needs no staging
   protocol, and point loads stay well under a millisecond with 16 clients.

## Embedded PostgreSQL

[PGlite](https://github.com/electric-sql/pglite) compiles PostgreSQL to
WebAssembly, ships pgvector, and is dual-licensed Apache-2.0 and PostgreSQL
License, so a local install could keep every PostgreSQL feature. Its
supported runtimes are JavaScript ones (browsers, Node, Bun, Deno), and its
socket server for other clients is a Node program. Two Go projects run its
WASI build under wazero, the runtime ADR 9 already uses:
[wasipg](https://github.com/moznion/wasipg) (pre-alpha, pgx over the wire
protocol, plpgsql only, one connection per engine) and
[gopglite](https://pkg.go.dev/github.com/bobTheBuilder7/gopglite) (marked
"do not use" by its author). Neither shows pgvector working. This spike did
not run them. A follow-up spike should, before Bearing promises a
single-binary install.

## Caveats

- One machine, default settings, nothing tuned. Read the numbers as orders
  of magnitude.
- SurrealDB was driven over HTTP and JSON, where surrealstore uses WebSocket
  and CBOR, so client overhead differs. The query plans and the
  conformance timings don't depend on the client.
- The "engine alone" column times whole conformance cases (building the
  ChangeSets, the refused apply, the apply and the read-back), not the apply
  alone, so it overstates the engine's share.
- PostgreSQL timings include starting `psql`. ClickHouse point reads could
  improve with a smaller `index_granularity`, at some cost to its scans.
- The rows are synthetic but follow surrealstore's layout; a `pgstore`
  would choose its own layout and could do better.

## Reproducing

```sh
cd spikes/storage-engines
mkdir -p /tmp/se
python3 gen.py seed 200000 > /tmp/se/seed.tsv
python3 gen.py apply 50000 > /tmp/se/apply.tsv
PGHOST=/tmp PGPORT=5433 PGUSER=postgres ./postgres.sh /tmp/se
./clickhouse.sh "$(command -v clickhouse)" /tmp/se
(cd surrealdb && go run . -data /tmp/se)
```

Each script expects its server on the default local port (PostgreSQL as
set by the `PG*` variables, ClickHouse on 9000, SurrealDB on 8000 as
`root`/`root`).
