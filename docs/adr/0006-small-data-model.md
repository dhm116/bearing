# 6. A small data model: observations, entities, facts

Date: 2026-10-20 · Status: accepted

## Context

The pre-pivot graph contract has 17 methods: bitemporal reads on two time
axes, alias binding timelines, merges and un-merges with reviews, supports,
resolver state, conflicts, data-quality issues, and backup and restore. A
6,000-line resolver turns observations into change sets that the store
checks against those rules. Porting that to DuckDB would rebuild the
complexity the pivot set out to leave.

The MVP needs to answer five questions with evidence (`AGENTS.md`). The
observation format adapters emit (`proto/bearing/model/v1alpha1/observation.proto`)
and its validation in `pkg/model` already serve that and stay.

| Option | Pros | Cons |
| --- | --- | --- |
| Port the contract to DuckDB | Resolver, queries and server keep working | Keeps every pre-pivot rule; DuckDB is a poor fit for row-by-row rule checks |
| Small model, resolution inside the store's write | A few tables and one write path; each MVP question is one or two SQL queries | Rewrites the resolver, `pkg/query` and the server's apply step |

## Decision

Four tables, written in one transaction per batch:

| Table | Holds |
| --- | --- |
| `observation` | Every validated observation as received: deterministic ID, source, kind, key, observed time, payload. Append-only; a repeated ID is skipped. This is the provenance |
| `entity` | One row per thing (a service, a person, a team), with its kind, a display name and `merged_into` when two turn out to be the same |
| `alias` | Every key that names an entity (`github:repo/acme/payments`, `email:person/ana@acme.com`), the source that said so, and when first and last seen |
| `fact` | Subject, predicate, object (an entity or a value), `asserted` or `proposed`, confidence, `valid_from`/`valid_to`, and the observation IDs behind it |

- **Identity.** An observation's key and aliases bind to one entity. When
  one observation names keys already bound to two entities, the newer
  entity is merged into the older (`merged_into`); reads follow it. There
  is no un-merge in the MVP.
- **Facts.** A relation or attribute in an observation opens a fact or
  refreshes it. It closes (`valid_to`) when an observation says it is
  absent, or when a complete snapshot from the same source no longer
  contains it. Deterministic sources assert; inferred links are
  `proposed` and never answer ownership or dependency questions.
- **Time.** One axis: when the source observed it. "What changed between
  X and Y" is the facts opened or closed in that window. Late-arriving
  data that rewrites history is an edge case, not a design input.
- **Interface.** `graph.Graph` (named `graph.Store` when this was written) has `Ingest(ctx, source, observations)` and the
  reads the questions need: `Entity`, `Find`, `Facts` (by subject,
  predicate, value), `Walk` (recursive over a predicate, both directions,
  with the facts on each step) and `Changes`. Resolution happens inside
  `Ingest`, so a replacement store implements the same rules.
- The event log the server uses for webhooks and sync requests moves to a
  DuckDB table with the same contract.

## Consequences

- `internal/memstore`, `pkg/resolver`, `pkg/contracts`' graph contract and
  conformance suite, and the resolver's proto state are deleted when the new
  store is wired in.
- Lost for now: two time axes ("as recorded at"), un-merge, merge reviews,
  conflict and data-quality reports, backup and restore (a DuckDB file is
  copied instead). Each goes in `docs/edge-cases.md` or the roadmap.
- `pkg/query` and the CLI answer from `Store`; the API's routes stay.
