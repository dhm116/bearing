# 2. Facts carry their provenance; there is no audit log

Date: 2026-10-20 · Status: accepted

## Context

Bearing answers questions such as "which team owns this service?". An answer
is only useful if a person can see why Bearing believes it. Before the
pivot this was split between a graph and a tamper-evident audit log with
signed checkpoints and retention, which cost more than it returned.

## Decision

- Adapters emit **observations**. The core resolves them into **entities**
  and **facts** (subject, predicate, object).
- Every fact records the observations that support it, their sources, a
  confidence and the time it was observed. That record is the provenance;
  there is no separate audit log, hash chain or checkpoint file.
- Answers that decide something (ownership, dependencies) read only
  **asserted** facts. Rules that are deterministic (CODEOWNERS, Kubernetes
  labels, package manifests) assert facts directly. Anything inferred
  (models, heuristics) is stored as a **proposed** link with its evidence
  and never answers those questions until promoted.
- Indexes (search, embeddings) are derived and can be rebuilt from the
  facts.

## Consequences

- Every answer can show "because of these observations from these sources".
- Tamper evidence and signed history are out of scope. If they are needed
  later, they build on provenance (see the roadmap).
