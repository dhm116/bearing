# 8. Keep the DuckDB file; DuckLake later, not now

Date: 2026-10-23 · Status: accepted

## Context

ADR 5 left room to store the graph in a DuckLake when one DuckDB file is
not enough. A spike put the store on DuckLake with a SQLite catalog and
local Parquet files and measured it
([report](https://github.com/dhm116/bearing/blob/spike/ducklake/docs/spikes/ducklake.md)).

| Option | Pros | Cons |
| --- | --- | --- |
| One DuckDB file (today) | Reads in 1–3 ms; `make test` in ~10 s on the store we ship | One writing process; DuckDB crashes under concurrent read and write, so the store uses one connection |
| DuckLake, SQLite catalog | Several readers and writers, no duplicates in tests; a path to PostgreSQL catalogs, S3 and time travel | Every query 10–25× slower (≈3 ms catalog lookup each, reads 19–30 ms); a catalog snapshot per event-log operation; SQLite needs WAL set by editing its file header; `make test` over 2 minutes; 27 MB extensions per platform to pin and ship |

## Decision

- Keep the single DuckDB file as the store (ADR 5 stands).
- Don't move the event log into a lake, ever: if we adopt DuckLake, the
  event log stays in a DuckDB file beside it.
- Readers in other processes use the server's API (`bearing get --server`),
  not the store file.
- Revisit when we need several writing processes, object storage or "as of"
  reads from snapshots, starting from the spike's "If we adopt it later"
  list.

## Consequences

- Reads and tests stay fast, and tests keep running on the production
  store.
- One server writes; scaling out waits for this ADR to be superseded.
- The single-connection workaround for DuckDB's concurrency crash stays
  (`docs/edge-cases.md`). The spike saw no crash on DuckLake in 13 runs,
  which hints the bug is in DuckDB's storage layer, but proves nothing.
