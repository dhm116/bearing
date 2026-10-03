# 2. The graph is the source of truth; the vector index is derived

Date: 2026-09-28 · Status: accepted, amended by data-model v0.3

## Context

Bearing builds a semantic map of the organization. A vector store such as
Qdrant is good at fuzzy questions ("what handles refunds?"). Ownership
answers and policy decisions ("may this person roll back payments-api?") need
exact, auditable facts.

## Decision

- Store entities and facts in a graph (`GraphStore`). Every fact carries its
  sources, a confidence score and history.
- Keep a semantic index (`VectorIndex`) beside it. Every point in the index
  references a graph entity ID. The index can always be rebuilt from the graph.
- Policy and ownership answers read only asserted graph facts. Queries that
  start with semantic search verify their results in the graph.

## Consequences

- A fuzzy match can never grant access or page the wrong team.
- Two stores to run in production. For small installs, PostgreSQL with
  pgvector can back both interfaces.
- Superseded in part by [ADR 5](0005-one-store-to-start.md): one SurrealDB
  database backs both interfaces by default.

## Amended by data-model v0.3

The [data model](../spec/data-model.md#state-determinism-and-apply) splits
Bearing's state in two. The **identity store** (subjects, alias bindings,
merge records) is primary state: backed up with the graph and never rebuilt,
because identity decisions depend on apply order and the event log keeps
only a window (ADR 7). Facts and their history (the claim store) and the
vector index stay rebuildable, against that identity store.
