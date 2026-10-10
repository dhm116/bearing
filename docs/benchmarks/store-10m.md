# The PostgreSQL store at the design target

Measured with `cmd/bearing-bench` ([method](README.md)) for
[#135](https://github.com/dhm116/bearing/issues/135), on 2026-10-10, against
`internal/pgstore` at commit `3cd5fe3` (the SurrealDB removal). The `related` read fix described below was made afterwards and measured on the same store.

## Answers

**Can it hold 10 million facts?** The run reached **3.0 million fact rows**
(12.2 million rows in all, 13.6 GB), not 10 million. The machine's disk
allowance, about 28 GB free, cannot hold the 45 GB that 10 million need, so
the 10 million figures below are projections, and a rerun with
`make bench` on a bigger disk is the way to replace them. The two measured
sizes, 1.0M and 3.0M, grow in a straight line (4.6 and 4.5 KB per fact row),
and nothing the benchmark measured grows faster than the data except the
resolver's watermark state (#136) and the reads listed under "Unbounded".
PostgreSQL itself is comfortable at 45 GB.

**What do ordinary questions cost at 3M fact rows?** Questions that name a
repository, a key or an alias are flat as the store grows: `ResolveKey` 1.8 ms
(p50), `AsOf` of a repository 5.3 ms, `bearing get <repository>` 37 ms.
Questions about a person cost more as the person's history grows: `AsOf` of a
person 27 ms (11.7 ms at 1M), `bearing get <person>` 122 ms (70 ms before the `related` fix; 108 ms at 3M).

**Where it goes wrong:**

1. *Unbounded reads* ([#167](https://github.com/dhm116/bearing/issues/167)).
   `bearing related <person> --predicate changed_by` took 64 s and 7.2 GB
   for a person with about 190 Changes, and an every-predicate `related` of a
   repository passed a 6 GB heap limit, because the store loaded every fact
   with the predicate to find the ones pointing at a subject. **Fixed in this
   change** (a missing use of the subject index, a few lines in
   `internal/pgstore`): 1.0 s and 9.5 MB, and 21 ms. Still open: the whole-org
   `changes --since`, which passes 6 GB and needs a result limit.
2. *The resolver* ([#168](https://github.com/dhm116/bearing/issues/168)).
   One new Change costs 220 database transactions at 3M fact rows (49 at 12k);
   one pipeline ingests 3.3 events/s, 45 minutes for a full re-read of the
   generated org.
3. *Concurrent writers* ([#170](https://github.com/dhm116/bearing/issues/170)).
   One writer lands 65 ChangeSets/s; 16 land 21/s, because stale attempts
   hold the head row lock.
4. *Space* ([#169](https://github.com/dhm116/bearing/issues/169)). 4.5 KB per
   fact row, about 20 times the weight of the facts and supports; resolver
   state is the biggest part.
5. *Watermarks* ([#136](https://github.com/dhm116/bearing/issues/136)),
   quadratic in the number of syncs; numbers posted there.

**Decisions the issue asked for**

- *Do series need to be loadable as of one record time (the remaining
  C-STORE-9 exception)?* **No, not for this target.** The cost of a read is
  the number of series that name the subject, not the depth of one series. A
  repository whose description changes at every read (about 500 version rows
  of that one fact after 250 reads, two per read) answers `AsOf` in 14 ms
  against 5 ms at the same sync count with none, and
  `ResolveKey` stays at 1.5 ms. A person's reads grow because 190 Changes name
  them, and loading each series as of one record time would load the same
  190 series. What helps is bounding what a read returns (#167). The per-apply
  cost that does grow with history is the resolver's, and that is the
  watermark state (#136) and batching (#168), not the series layout.
- *Is the per-partition hybrid clock relaxation needed?* **No.** One writer
  lands 65 ChangeSets/s at 3M fact rows (about 5.6 million a day) against a few
  thousand events a day for thousands of repositories; even with the
  resolver in front, 3.3 events/s is 285,000 a day. The single apply clock is
  not the limit; the resolver is. Parallel writers lose to one (#170), so
  ingest should stay a single consumer.

## Setup

| | |
| --- | --- |
| CPU, memory | 4 vCPU Intel Xeon @ 2.80 GHz, 16 GB |
| Kernel, Go | Linux 6.18.44, go 1.27.2 |
| PostgreSQL | 16.15 (Ubuntu), same host as the benchmark, over loopback TCP |
| Settings | `shared_buffers` 3GB, `effective_cache_size` 10GB, `work_mem` 32MB, `max_wal_size` 8GB (3GB from the 1M measurement on, to fit the disk), `fsync` and `synchronous_commit` on, `autovacuum` on, `pg_stat_statements` loaded |
| Store | `postgres://` through `pkg/store`, role without administrator rights, schema of its own |
| Org | 5,000 repositories, 3,000 people, 300 teams at the first read, 60% of people with a directory account (each a merge, 1,800 merge records), 1,500 merged pull requests a day, 25 repository edits, 4 CODEOWNERS changes, 8 membership changes, 1 rename and 1 new repository a day, a full re-read every 120 days, seed 1 |

The load ran 626,750 events over 384 simulated days. The first five days went
through the resolver, as ingest would; after that Change events were applied
as the ChangeSet the resolver writes for them (a test proves the two are
identical), because the resolver is too slow to reach millions of facts in a
day (#168). The whole run took about 5.5 hours for 3.0M fact rows, measurements included. Bulk applies
(one ChangeSet each) took 6-8 ms at the start and 9-13 ms at 3M fact rows.

Read timings are 8-second runs per caller count on the Go side (a call is the
store method or the `pkg/query` function the CLI command runs, not a process
start). Memory is the Go allocation per call; for the reads that did not
finish, the benchmark stops at a 6 GB heap. The database had no other client.

## Space

| Fact rows | Version rows | Size on disk | Per fact row |
| --- | --- | --- | --- |
| 1,004,159 | 4.1M | 4.63 GB | 4.6 KB |
| 3,003,916 | 12.2M | 13.64 GB | 4.5 KB |
| 10,000,000 (projected) | ~41M | ~45 GB, plus WAL | |

At 3.0M fact rows: `version` 5.4 GB (+0.95 GB key), `series` 2.7 GB (+0.88 GB
key), `series_subject` 0.79 GB (+1.25 GB key, the largest index), `journal`
1.34 GB (627,148 applies, 2.1 KB each), `subject` 120 MB. By content, the
resolver's state rows are 3.1 GB (`sup/` confirmations 2.1 GB, `wm/` 1.0 GB),
supports 645 MB, the `unobserved_object` issues 643 MB and the facts
themselves 65 MB. Details and suggestions in #169.

## Reads

p50 / p99 in milliseconds, one caller and 16 callers at the same time. 1M and
3M are fact rows.

| Read | 1M, 1 caller | 1M, 16 | 3M, 1 | 3M, 16 | MB allocated per call (3M) |
| --- | --- | --- | --- | --- | --- |
| `ResolveKey` (now) | 1.6 / 2.9 | 6.5 / 12.2 | 1.8 / 4.0 | 7.2 / 13.8 | 0.01 |
| `ResolveKey` (alias) | 1.7 / 3.2 | 6.8 / 12.4 | 1.9 / 4.3 | 7.9 / 15.9 | 0.01 |
| `ResolveKey` (record time mid-history) | 1.7 / 2.8 | 7.0 / 12.6 | 2.0 / 4.4 | 7.3 / 14.0 | 0.01 |
| `AsOf` a repository (now) | 4.5 / 7.5 | 19.1 / 33.4 | 5.3 / 10.2 | 22.6 / 37.7 | 0.2 |
| `AsOf` a repository (record time mid-history) | 4.4 / 7.2 | 20.0 / 33.4 | 4.7 / 8.8 | 19.6 / 33.1 | 0.2 |
| `AsOf` a person (now) | 11.7 / 17.9 | 55 / 83 | 27.4 / 44.1 | 118 / 181 | 2.0 |
| `Changes` of a repository, 30 days | 4.6 / 8.4 | 19.6 / 32.3 | 4.8 / 8.9 | 21.2 / 34.2 | 0.2 |
| `Changes` of a person, 30 days | 11.5 / 17.3 | 55 / 86 | 24.1 / 34.0 | 129 / 195 | 2.8 |
| `bearing get <repository>` | 32.5 / 43.4 | 146 / 187 | 36.7 / 48.6 | 156 / 195 | 1.3 |
| `bearing get <repository> --as-of` (record time in the past) | 32.9 / 46.2 | 141 / 185 | 32.9 / 42.9 | 157 / 211 | 1.3 |
| `bearing get <person>` | 69.5 / 99.9 | 325 / 494 | 108 / 169 | 489 / 764 | 7.5 |
| `bearing get <change>` | 31.4 / 43.5 | 134 / 172 | 51.1 / 71.9 | 204 / 265 | 2.2 |
| `bearing owner <repository>` | 25.2 / 34.2 | 119 / 157 | 26.6 / 35.5 | 129 / 180 | 1.3 |
| `bearing changes --since 1 day <repository>` | 10.5 / 15.3 | 48 / 66 | 15.8 / 29.0 | 62 / 108 | 0.4 |
| `bearing related <repository> --predicate approves_changes` | 440 / 494 | 2,795 / 4,298 | 497 / 534 | 3,616 / 5,411 | 88 |
| `bearing related <team> --predicate member_of` | 680 / 774 | 3,913 / 6,547 | 1,398 / 1,437 | 6,516 / 9,420 | 145 |
| `bearing related <person> --predicate changed_by` | 12,469 | stopped at 6 GB | 64,331 | stopped at 6 GB | 7,216 |
| `bearing related <repository>` (every predicate) | 69,416 | not run | stopped at 6 GB | not run | |
| `bearing changes --since 1 day`, whole org | stopped at 6 GB | not run | stopped at 6 GB | not run | |

The four `related` rows above were measured before the fix in this change.
The same store afterwards, same machine:

| Read | 1 caller, p50 / p99 ms | 16 callers | MB per call |
| --- | --- | --- | --- |
| `related <repository> --predicate approves_changes` | 21 / 30 | 99 / 134 | 0.13 |
| `related <team> --predicate member_of` | 541 / 771 | 1,702 / 2,758 | 1.7 |
| `related <person> --predicate changed_by` | 1,040 / 1,426 | 4,035 / 4,599 | 9.5 |
| `related <repository>` (every predicate) | 21 (3 calls) | not run | 0.39 |
| `bearing get <repository>` | 27.8 / 40.0 | 119 / 162 | 0.3 |
| `bearing owner <repository>` | 19.2 / 28.9 | 85 / 120 | 0.11 |

The `ResolveKey`, `AsOf` and `Changes` rows repeat within noise
(`results/2026-10-10/related-fix.ndjson`). The fix also reaches `bearing get`:
`get <change>` falls from 51 to 37 ms (p50) and from 2.2 MB to 0.2 MB per
call, and `get <person>` allocates 2.3 MB per call instead of 7.5 MB but is
not faster (109 ms before, 122 ms after; 16 callers 489 and 436). What is left
in `related <person>` is one lookup of a name per result (#167).

With 16 callers on 4 cores, latency is about four times the single-caller
latency: the cores are full, so throughput stops growing at about 4 callers
(`ResolveKey` 2,100 calls/s with 16 callers, 520 with one). Memory per call is
small except for the rows marked in #167.

PostgreSQL's own plans for the store's load queries (`EXPLAIN (ANALYZE,
BUFFERS)`, 3M fact rows): the alias lookup 0.04 ms, a repository's facts 0.28
ms and their versions 0.42 ms, a person's facts 19 ms and versions 28 ms, the
merge components of a subject 1.1 ms. All use the primary keys and
`series_subject`; no missing index. The slowest is "every fact with one
predicate": 9 ms for the series and 547 ms for their versions. In the
statement statistics the highest means were the two unfiltered whole-org
reads (26.6 s and 12.3 s, two calls each), then the predicate-only series
query at 488 ms (186 calls, 7.0M rows returned); these are the unfiltered
shapes #167 bounds. A repository read takes 33-37 ms through the CLI against 5 ms through
the store because `Get` makes several reads.

## Applying

Writers built ChangeSets ahead (store level) or handed events to the resolver
(resolver level); each group ran for 20 seconds.

| Level | Writers | Applied / s at 1M | at 3M | Stale attempts at 3M | Landing time p50 / p99 at 3M |
| --- | --- | --- | --- | --- | --- |
| Store | 1 | 69 | 65 | 0% | 15 / 27 ms |
| Store | 4 | 54 | 44 | 75% | 22 / 34 ms |
| Store | 16 | 20 | 21 | 92% | 433 ms / 3.35 s |
| Resolver | 1 | 9.6 | 3.3 | 0% | 294 / 379 ms |
| Resolver | 4 | 8.5 | 3.1 | 74% | 658 ms / 3.15 s |
| Resolver | 16 | 2.9 | 1.1 | 92% | 2.2 / 11.4 s |

The head row lock serializes applies correctly; the cost of waiting is in
#170. The resolver cost of one event (dry run, 30 events of each kind) at 3M
fact rows:

| Event | p50 | Transactions | Rows returned |
| --- | --- | --- | --- |
| New Change by a person | 299 ms | 220 | 20,660 |
| Repository read again, unchanged | 150 ms | 95 | 21,022 |
| Team read again, unchanged | 759 ms | 230 | 257,100 |
| Person read again, unchanged | 315 ms | 206 | 50,716 |

After the read fix in this change the rows per event fall (11,481, 11,641,
39,164 and 15,274) and the team event takes 445 ms; the number of
transactions stays at 203, 108, 222 and 224, which is what #168 is about.

## Merges

Reads and applies with 1,000, 10,000 and 100,000 synthetic merge records
(pairs of people nothing else refers to, plus one survivor of 250 merges),
over a 300-repository org. The `component` table, which names each subject's
merge group, keeps them flat. (The merges are pairs and one large group, not
facts spread across many large groups; see the limits below.)

| Merge records | `ResolveKey` p50 | `AsOf` a merged person p50 | `Merges` of the 250-merge survivor p50 | `Subject` of it p50 |
| --- | --- | --- | --- | --- |
| 1,000 | 1.6 ms | 5.8 ms | 3.0 ms | 4.8 ms |
| 10,000 | 1.9 ms | 7.0 ms | 5.0 ms | 6.3 ms |
| 100,000 | 1.6 ms | 7.5 ms | 3.8 ms | 4.9 ms |

Store-level applies with one writer ran at 118, 238 and 329 per second, so
more merge records did not slow them down. The rise itself is unexplained; I
did not isolate its cause.

## Series history against sync count

A ten-repository org read again and again, nothing changing, with the #77 fix
(`history`), and with one repository's description changing at every read
(`history --flip`):

| Syncs | Mean apply per event | Resolve one repository (p50) | State bytes | Flip: `AsOf` of the repository | Flip: fact version rows (whole org) |
| --- | --- | --- | --- | --- | --- |
| 25 | 23 ms | 78 ms | 13.5 MB | 6.2 ms | 235 |
| 100 | 74 ms | 137 ms | 168 MB | 9.8 ms | 385 |
| 250 | 172 ms | 248 ms | 993 MB | 14.3 ms | 685 |
| 500 | 439 ms | 584 ms | 3.89 GB | | |

`AsOf` and `ResolveKey` do not depend on sync count (3-5 ms and 1.0-1.9 ms from 1
to 500 syncs). What grows is the resolver's `wm/` watermark entries, 98% of the
state at 500 syncs, quadratic in syncs: this is the design question of
[#136](https://github.com/dhm116/bearing/issues/136). The sync-count and
resolve rows come from the unchanged-org run; the flip columns from the run
with a changing description.

## Limits of this run

- The merge rows test pairs of people nothing else refers to and one group of
  250; they do not test facts that point at members of many large groups.
- Some figures come from ad hoc queries against the loaded schema, not from a
  committed measurement: the split of state bytes (`sup/` 2.1 GB, `wm/` 1.0 GB,
  supports 645 MB, `unobserved_object` issues 643 MB, facts 65 MB), the 45
  minutes for a full re-read, and "throughput stops growing at about 4
  callers" (the runs used 1 and 16 callers; the four-core ceiling is inferred).

- One host: the database shared 4 cores and 16 GB with the benchmark. Larger
  hardware changes the numbers, not the shapes.
- Not 10M (see above). Per-person costs rise with the person's history; at
  10M that is about 600 Changes per person, so `AsOf` of a person would be
  roughly 3x the 3M figure.
- The bulk path skips the resolver for ordinary Changes after day 5, so the
  load's own events per second are not an ingest rate. Resolver-level rows
  above are the ingest rate.
- `pg_stat_statements` was not read at 1M (the benchmark's search path hid
  the extension; fixed before the 3M run).
