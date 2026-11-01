# 10. Configured, namespaced kinds beside the core kinds

Date: 2026-11-01 · Status: proposed

## Context

The kinds and relation predicates are a closed list in
`pkg/model/registry.go`. Adapters may declare their own attributes, but a
declaration naming any other kind is rejected, so an adapter for a new
domain (Vault policies, operating-system services) has nowhere to put its
records. Names also collide across domains: a Kubernetes `Service` and an
operating-system `Service` are different things.

Every check goes through `Kind.Valid` or `LookupPredicate`, from
declaration and observation validation, ingest, settings and two queries.

| Option | Pros | Cons |
| --- | --- | --- |
| Grow the core list in code | One place, reviewed | Every new domain waits on a Bearing release; the core stops being small |
| Adapters define their own kinds | No operator step | Each adapter gets its own `Secret`, so queries can't treat them as one thing; meaning changes with whichever adapters run |
| Operators configure kinds that adapters use | Shared kinds, an explicit opt-in, the core stays small | A config resource to validate; the store must remember kinds after config drops them |
| Infer mappings at startup with a model | No authoring | A restart can change answers with no new evidence (ADR 2) |

## Decision

- **Two tiers.** Core kinds stay in code. Operators add kinds with a new
  config resource beside `Source`, giving each kind its name, an optional
  parent, its attributes (type, cardinality, conflict policy) and its
  relations (domain and range). Ingest accepts the union of the two.
- **Namespaces name the owner.** A configured kind is `<namespace>.<Kind>`,
  as in `k8s.Service` or `systemd.Service`. The namespace names the
  vocabulary that defines the kind, not a category, and nothing reads
  meaning from it. Core kinds keep their bare names (`Person` is
  `bearing.Person`). The separator is `.` because keys already use
  `issuer:type/value`.
- **One parent.** `extends` names a core kind or a configured kind; chains
  end at the core or at nothing, and cycles are rejected. A kind with no
  parent works with `Get`, `Related`, `Find` and `Changes` but never
  answers the five MVP questions. Core questions about a kind include its
  descendants.
- **A kind says what a thing is, not where it came from.** Provenance
  already records the source. An adapter picks the kind for each
  observation: a Vault entity is emitted as a `Person`, a service account
  or a group, depending on what the entity is. When the adapter can't
  tell, it emits an abstract parent. The core gains abstract kinds only
  when an adapter needs one; the first is likely `Principal`, the parent
  of `Person`, `Team` and a service account.
- **Opt-in is checked at `Describe`.** Each declared kind may be marked
  `optional`; the default is required, so existing declarations keep
  today's behavior. A required kind that is neither core nor configured
  rejects the declaration, with a message naming the kind to enable. An
  optional one is accepted but inactive: Bearing logs the skipped kinds
  once at boot, and at ingest drops and counts (per source and kind) its
  observations and any relation whose other end is an inactive kind.
  Enabling it later takes a full sync from that source, since its
  observations can't be validated before its definition exists. Adapter
  bundles may ship a suggested config snippet. Mappings are written by
  people and reviewed; a model may suggest them while an adapter is being
  written, never at run time.
- **Bearing tells adapters what it will keep.** `SyncRequest` and
  `HandleRequest` carry `enabled_kinds`, read from config on every call,
  so an adapter can skip API calls for kinds Bearing would drop. Using it
  is up to the adapter; Bearing still drops inactive kinds at ingest, so
  an adapter that ignores the field stays correct. Adapters never call
  Bearing for it, which keeps them stateless and the calls one-way
  (ADR 4).
- **Clusters follow one line of descent.** ADR 7's "never across kinds"
  becomes: two records may join only when one's kind is the other's or an
  ancestor of it. A `Principal` can join a `Person`; a `Person` never joins
  a `Team`.
- **The store keeps the definitions.** At boot, the merged vocabulary is
  written to a `kind` table. A kind dropped from config is marked retired,
  not deleted. Ingest rejects observations of retired kinds; reads resolve
  the hierarchy and attribute shapes from the table, so stored facts of a
  retired kind still answer, under their parents too. Queries reject only
  kinds the table has never held.
- **Configured kinds only grow.** Retyping an attribute or renaming a kind
  means defining a new kind.

## Consequences

- An operator can add a domain without a Bearing release, and two
  adapters in one domain share its kinds.
- `model.Kinds` and the package-level predicate map become a
  `*model.Registry` built at boot from core plus config; validation, the
  store and queries take it. Tests build their own, so they stay parallel.
- The cluster check in `internal/duckstore/links.go`, which compares kinds
  for equality, compares lines of descent instead, and a set takes its most
  specific kind.
- A record whose kind narrows later (`Principal` to `Person`) needs a rule;
  it goes in `docs/edge-cases.md` until an adapter emits abstract kinds.
- An adapter upgrade that adds an optional kind runs at once; one that adds
  a required kind waits for the operator to enable it.
- `proto/` gains `optional` on the declared kind and `enabled_kinds` on the
  two requests; `pkg/adapter` passes the list to adapters.
- An introspection endpoint can list the vocabulary from the `kind` table,
  with each kind's parent and whether it accepts data.
