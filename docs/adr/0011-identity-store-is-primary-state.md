# 11. The identity store is primary state

Date: 2026-10-03 · Status: proposed

## Context

[ADR 2](0002-graph-is-source-of-truth.md) promises that derived data can be
rebuilt, and [ADR 7](0007-durable-event-log.md) says replaying the event log
rebuilds the graph. The [data model](../spec/data-model.md) shows that this
holds only in part:

- Identity decisions (which subject an alias belongs to, mints, merges and
  un-merges) depend on the order events are applied, and the log orders
  events only within a partition. Replaying cannot reproduce them, so
  subject IDs would change.
- The event log keeps raw events for a window (default 30 days). Facts
  recorded from events older than that cannot be replayed.
- Manual events (merges, overrides, withdrawals) can't be got back from a
  full resync of the sources.
- Tiered compaction replaces old history with summaries, which can't be
  expanded again.

| Option | Pros | Cons |
| --- | --- | --- |
| Keep every event forever and rebuild everything | One source of truth | Unbounded log; still order-dependent identity |
| Back up identity, old history and manual effects as primary state | Stable subject IDs; bounded log | Backups become essential; less can be rebuilt |

## Decision

- The **identity store** (subjects, alias bindings, merge and un-merge
  records) is primary state. It is backed up with the graph, never rebuilt
  from events, and never compacted.
- The **claim store** is rebuilt by replay only for valid and record times
  inside the event log's retention window. History older than that,
  compaction summaries and the effects of manual events are primary state,
  backed up with the graph.
- Manual events are exempt from the log's retention window: they are kept
  as long as their effects are live.
- The vector index stays derived and rebuildable from the graph.

## Consequences

- Backup and restore of the graph store are required operations, not
  optional ones, and need a conformance test.
- Disaster recovery is restore from backup, then replay of the retained
  window. A full resync alone no longer recovers identity.
- Supersedes in part ADR 2 (what can be rebuilt) and ADR 7 (replay rebuilds
  the graph; retention of manual events).
