# Apply throughput with the audit log on and off

Measured with `cmd/bearing-bench` ([method](README.md)) for
[#141](https://github.com/dhm116/bearing/issues/141), on 2026-10-10 and 11,
against `internal/pgstore` at commit `4f5a14a` (before the `bearing server`
work). The issue asks for at least 200 events per second on PostgreSQL, with
the audit log on and off, recorded and not gated. The raw lines are in
[`results/2026-10-11/audit.ndjson`](results/2026-10-11/audit.ndjson).

## Answers

**Does `Apply` reach 200 events per second?** No. One consumer applying Changes
one after the other lands about **145 per second with the audit log off and
about 130 with it on**, and the rate does not change between 0.1 and 3.0
million fact rows (see the table). Counting only the call to `Apply`, which is
what a server's loop pays, it is about 155 and 140. That is 70 to 75% of the
target at best. The target is also well above what the org needs: the design
target of 5,000 repositories and 3,000 people produces about 1,500 merged pull
requests a day, and 130 per second is 11 million a day. The shortfall is a
cost per `Apply`, and it would matter when a source is read again from scratch
and the whole org arrives at once (a full read of the generated org is about
8,300 events, a minute at this rate).

**What does the audit log cost?** About **0.7 to 1.0 ms on each `Apply`**, 11 to
15% of the call (6.1 ms at the median with the log off, 6.7 to 7.2 ms on), and
it does not grow with the log: the cost at 4.5 million records is the cost at
180,000. In space it is large: **811 bytes a record**, 20% of the store at 3.0
million fact rows (3.7 of 18.1 GB), so about 12 GB of a projected 60 GB at 10
million fact rows. The cost is writing records, not chaining them: SHA-256 over
all the records of the profile run was 0.5 s of 54 s of CPU.

**Are per-partition chains needed (ADR 8's fallback)?** No. The chain adds no
waiting of its own: it is extended under the same head row lock every apply
already takes, so a chain per partition would let applies overlap only if the
head lock did too, and the 2026-10-10 benchmark found that parallel writers
land fewer events than one ([#170](https://github.com/dhm116/bearing/issues/170)).
The log adds under a millisecond to a 6 ms call; what keeps `Apply` from 200
a second is the other 6 ms (below). ADR 8 records this.

**Where do the 7 ms go?** Not in any one place. For each `Apply` the store runs
20 statements one after the other (18, `BEGIN` and `COMMIT`). Adding up what
was measured for a call with the log on (7.1 ms): the database spends about
1.8 ms executing them, the process about 2.1 ms of CPU (the reference rules in
the scratch store, encoding, reading rows, TLS), the 20 round trips about
1.2 ms (0.058 ms each, loopback with TLS), and the commit's flush about
0.2 ms. That is 5.3 of 7.1 ms; the remaining 1.8 ms (waiting between
statements, scheduling) was not measured. Both the round trips and the flush
are the parts a real deployment makes worse (below).

## What was measured

One writer, as the server's apply loop will be: it holds the head itself, so
there are no stale attempts and no lock waits. Each run applies new Change
events for 30 seconds; the Change's ChangeSet is the one the resolver writes
(a test proves that), with its `audit` field filled or empty. Three pairs of
runs at each size, off and on in alternating order so that growth and drift
fall on both. The store was loaded by the benchmark's normal path: audit
records for every ChangeSet, 4.5 million of them at 3.0 million fact rows.

| Fact rows | Audit records | `Apply` mean, off / on (ms) | p50 | p99 | Events per second, off / on | `Apply` calls per second, off / on |
| --- | --- | --- | --- | --- | --- | --- |
| 102,918 | 0.18M | 6.53 / 7.39 | 6.3 / 7.1 | 10.6 / 11.5 | 130 / 125 | 153 / 135 |
| 302,783 | 0.38M | 6.43 / 7.43 | 6.2 / 7.2 | 10.2 / 11.6 | 143 / 125 | 156 / 135 |
| 1,006,665 | 1.33M | 6.30 / 7.20 | 6.1 / 7.0 | 10.1 / 11.2 | 146 / 128 | 159 / 139 |
| 3,002,712 | 4.52M | 6.46 / 7.14 | 6.1 / 6.7 | 11.3 / 12.9 | 144 / 130 | 155 / 140 |

"Events per second" counts the whole loop, which includes the benchmark
building the ChangeSet (about 0.55 ms, decoding a template); "`Apply` calls per
second" is one over the mean of the call alone. Each row averages three runs of
11,000 to 13,000 applies. Single runs vary by ±10% (at 3.0M the off runs were
127, 148 and 158 per second, and the first off run of a size was the slowest), so
differences under about 10 between rows are noise; the gap between off and on
shows in every pair of runs.

Each Change carries 7 audit records: a mint, a binding, and a status change
for each of its five facts.

**The audit entries are a stand-in.** The resolver does not yet write the
entries that [`data-model.md`](../spec/data-model.md#audit) lists; it writes
only the one for a dropped write (issue #218 adds the rest). So the benchmark
adds, to each ChangeSet, the entries the spec says the resolver will write,
derived from the ChangeSet (`cmd/bearing-bench/audit.go`): the same actions,
targets and embedded messages, so the records are about the size the real ones
will be. A resolver that audits more per event (conflicts, merges, overrides)
pays more per event, roughly 0.1 ms and 0.8 KB a record.

## Space

| | |
| --- | --- |
| `audit_record` heap | 2.85 GB for 4.52M records (630 B a record) |
| its five indexes | 0.82 GB (`target` 0.40, `actor` 0.18, primary key 0.10, `event` 0.09, `time` 0.05) |
| in all | 3.67 GB, 811 B a record, 20% of the schema (18.09 GB at 3.0M fact rows, against 13.64 GB without the log in the [store benchmark](store-10m.md)) |
| per Change event | 7 records, about 5.7 KB |
| projected at 10M fact rows | 15.1M records, about 12 GB, of about 60 GB |

The 10 million figures are projections from 3.0 million, as in the store
benchmark; the disk allowance stops there. A record's `data` column holds the
whole encoded record, including the before and after messages that the
`actor`, `target` and `action` columns repeat; trimming that is the place to
look if the size matters ([#239](https://github.com/dhm116/bearing/issues/239)).

## Compared with the store benchmark

The suite's own writer rows, taken at the same checkpoints with the log on,
agree: one writer lands 137 ChangeSets a second at 3.0M fact rows, against 65
in the [store benchmark](store-10m.md#applying). The store has changed since
that run (resolver state grows far less after
[#77](https://github.com/dhm116/bearing/issues/77) and
[#136](https://github.com/dhm116/bearing/issues/136), so a Change's ChangeSet
is smaller), and this machine's CPU is slower, not faster; I did not isolate
the cause of the gain. Four writers land 69 a second and 16 land 29, with 75%
and 92% of attempts stale, as before; one writer through the resolver lands
4.2 events a second at 3.0M. These rows are the `apply` lines of the raw file.

## What changes on another machine

- **Round trips.** 20 sequential statements a call are 1.2 ms at 0.058 ms
  each. With the database on another host (say 0.5 ms a round trip) they are
  10 ms and the call is about 15 ms, **65 events per second**. Fewer round
  trips is the most direct way to the target; it changes `internal/pgstore`
  and no contract ([#238](https://github.com/dhm116/bearing/issues/238)).
- **Commit latency.** Here a flush takes 0.2 ms (`pg_test_fsync`, 5,000 per
  second). On a network disk it is 1 to 5 ms, added to every call, as the
  flush cannot be shared with another writer.
- **TLS.** The benchmark's connection used TLS on loopback (the Ubuntu package's
  default). A deployment that requires it, as `verify-full` does, pays the
  same.

## Setup

As in the [store benchmark](store-10m.md#setup), except the CPU: 4 vCPU Intel
Xeon @ 2.10 GHz (2.80 GHz there), 16 GB, Linux 6.18.44, go 1.27.2, PostgreSQL
16.15 on the same host over loopback TCP with TLS, `shared_buffers` 3GB,
`max_wal_size` 3GB, `fsync` and `synchronous_commit` on. Same org (5,000
repositories, 3,000 people, 300 teams), seed 1, loaded with
`bearing-bench load --facts 3000000 --checkpoints
100000,300000,1000000,3000000`, which measures at each checkpoint (the load took
about 4 hours, its two full re-reads of the org included). The profile and the
statement counts come from a further run at 3.0M (`measure --only audit
--cpuprofile`).

## Limits of this run

- One host, one disk, the database sharing the cores with the benchmark.
- Not 10M fact rows (see above). The rate was flat from 0.1 to 3.0M; a deeper
  index at 10M adds well under a millisecond to an insert.
- The numbers are for `Apply`. The resolver in front of it takes 300 ms an
  event at 3M fact rows, so **ingest as a whole is about 3 events per second**
  until [#168](https://github.com/dhm116/bearing/issues/168) lands; the
  server, the event log and the audit entries the resolver will write are not
  in this measurement and are the next thing to measure once they land
  ([#240](https://github.com/dhm116/bearing/issues/240)).
- The statement times of the 100,000-row measurement include the earlier read
  measurements (the benchmark's role could not reset the statistics there);
  the other three sizes are clean.
- Everything here is one writer. Several writers were measured in the store
  benchmark (#170).
