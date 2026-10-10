# Store benchmarks

`cmd/bearing-bench` measures the PostgreSQL store ([ADR 14](../adr/0014-postgres-is-the-default-store.md))
at its design target: thousands of repositories and people and about ten
million facts. Results are in [`store-10m.md`](store-10m.md), with the raw lines under
[`results/`](results). This page says
how the benchmark works and how to run it again.

## What it does

A generated organization (`world.go`) feeds the store the way the sources
would. The first sync reads every repository, team and person from a
GitHub-like source and every account from a directory source, so the
resolver merges each linked person with their directory account. Then
simulated days pass: merged pull requests (the bulk of the facts), repository
edits, CODEOWNERS and team membership changes, renames, new repositories, and
a full re-read every 120 days. The generator is deterministic for a seed, so
a load that stops can resume from the store's journal.

Two paths feed the store:

- **Resolver path.** The first sync and the first simulated days go through
  `resolver.Apply`, exactly as ingest would.
- **Bulk path.** The resolver costs about 18 store read transactions per event
  (see the results), which is too slow to reach ten million facts in a
  working day. After `--bulk-after-day` days, a Change event is applied as the
  ChangeSet the resolver writes for it, built from a template. A test
  (`TestSyntheticChangeMatchesResolver`) checks that the template's ChangeSet
  is identical to the resolver's.

At each checkpoint (fact-row counts, `--checkpoints`) the benchmark vacuums
and analyzes the schema, then measures:

| Measurement | How |
| --- | --- |
| Table and index sizes, row counts | `pg_class`, `pg_stat_user_tables` |
| `ResolveKey`, `AsOf`, `Changes`, and the reads `bearing get`, `owner`, `related`, `changes --since`, `--as-of`, `--recorded-at` make | 1 and 16 concurrent callers; p50, p99, allocations, peak memory, blocks read |
| Slowest statements and their plans | `pg_stat_statements` (if loaded), `EXPLAIN (ANALYZE, BUFFERS)` of the store's load queries |
| Cost of resolving one event | dry-run `Resolve` of a new Change and of unchanged repository, team and person reads |
| Applies per second | 1, 4 and 16 writers, at the store (ChangeSets built ahead) and through the resolver; waiters on the head row lock; stale retries |

Two experiments need a store of their own: `merges` (1,000, 10,000 and
100,000 merge records) and `history` (series history against sync count).

## Running it

Use a PostgreSQL role that owns its schema and is not a superuser. For
`pg_stat_statements`, load the extension, let the role read it (`pg_monitor`)
and grant it `EXECUTE` on `pg_stat_statements_reset()`; without the extension
the statement and plan sections are left out, and without the grant they
include statements from before the measurement. The run creates one table,
`bench_meta`, in the store's schema to remember how far a load has got, and
the statement list leaves out statements that mention a password, a secret or
a role change.

```sh
export BEARING_STORE_PASSWORD=...
make bench BENCH='load --store postgres://bench@127.0.0.1:5432/bench?schema=main --facts 10000000 --out results.ndjson'
make bench BENCH='measure --store postgres://bench@127.0.0.1:5432/bench?schema=main --out results.ndjson'
make bench BENCH='merges  --store postgres://bench@127.0.0.1:5432/bench?schema=merges --out results.ndjson'
make bench BENCH='history --store postgres://bench@127.0.0.1:5432/bench?schema=history --out results.ndjson'
```

Run from the repository root (the reference declarations are read from
`testdata/declarations`). Nothing else should use the database during a
measurement: the PostgreSQL counters are database-wide. `--quick` shrinks
every measurement to under a second, to check the benchmark itself.
`go test ./cmd/bearing-bench` runs a smoke test of the load on `mem://`. The
full run is not part of `make check` or CI.

Results are JSON lines: `run` (hardware, PostgreSQL version and settings),
`rows`, `sizes`, `read`, `apply`, `resolve`, `statements`, `plans`,
`load_window`, and the experiments' own records.

Disk: a store takes about 4.5 KB per fact row, so 10 million fact rows need
about 45 GB for the tables plus the write-ahead log (set `max_wal_size` to
suit). The 2026-10-10 run stopped at 3 million for want of space.

`measure --only resolve` runs only the resolver timings with the database's
own transaction and row counters.
