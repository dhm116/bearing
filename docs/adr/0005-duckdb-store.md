# 5. DuckDB is the store

Date: 2026-10-20 · Status: accepted

## Context

Before the pivot, PostgreSQL backed the store, with a separate in-memory
backend for tests and a conformance suite keeping the two in step. That
meant two implementations of every rule and slow, service-dependent tests.

The MVP questions are a mix of scans over time ("what changed between X and
Y", "which services use language X below version Y") and short graph walks
("what does service X depend on").

| Option | Pros | Cons |
| --- | --- | --- |
| PostgreSQL | Proven here; recursive CTEs; multiple writers | Needs a server in tests; second in-memory backend to keep in step |
| DuckDB | Same engine in memory (tests) and on disk; fast scans; recursive CTEs; reads Parquet, CSV, JSON, S3 and other databases | One writing process; slow at many tiny writes; CGO |
| QuestDB | Fast time-series ingest | No graph walks; second database for relationships |

## Decision

- DuckDB, embedded in `bearing server` through `github.com/duckdb/duckdb-go`,
  holds the observation log, entities and facts. `:memory:` in tests, a file
  in production.
- One store implementation behind a narrow Go interface. Anything Bearing
  needs from a store is a method on that interface, so another engine can
  replace DuckDB; no second implementation is built now.
- Where the data lives is a deployment choice. DuckDB can keep it in a
  database that is attached through an extension (PostgreSQL, MySQL,
  SQLite) or in a DuckLake, while Bearing's queries stay the same.
- Writes are batched per sync page or webhook burst, never one transaction
  per observation.
- Graph walks use recursive CTEs.
- Full-text and vector search are deferred. Name lookup (for the MCP
  endpoint) uses `ILIKE` and the built-in `jaro_winkler_similarity` and
  `levenshtein` functions.
- Adapters remain the only way data enters. DuckDB's readers for external
  sources are for bulk imports and exports, not a path around adapters.

## Consequences

- Tests run against the real engine with no external services.
- One server process writes. Kubernetes runs one replica with a persistent
  volume. Paths to scale, if needed: DuckDB's
  [Quack](https://duckdb.org/docs/current/quack/overview) client/server
  extension (beta) and [DuckLake](https://ducklake.select/docs/stable/)
  (multiple writers over a SQL catalog and data files, with time travel).
- Requires CGO ([ADR 1](0001-go-apache-few-dependencies.md)).
- Follow-up: check whether DuckLake snapshots can answer "as of" questions
  before designing history tables by hand.
