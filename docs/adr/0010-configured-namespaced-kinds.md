# 10. Configured, namespaced kinds beside the core kinds

Date: 2026-11-01 · Status: proposed

## Context

The kinds and relation predicates are a closed list in
`pkg/model/registry.go`. Adapters may declare their own attributes, but a
declaration naming any other kind is rejected, so an adapter for a new
domain (Vault policies, operating-system services) has nowhere to put its
records. Names also collide across domains: a Kubernetes `Service` and an
operating-system `Service` are different things.

| Option | Pros | Cons |
| --- | --- | --- |
| Grow the core list in code | One place, reviewed | Every new domain waits on a Bearing release; the core stops being small |
| Adapters define their own kinds | No operator step | Each adapter gets its own `Secret`, so queries can't treat them as one thing; meaning changes with whichever adapters run |
| Operators configure kinds that adapters use | Shared kinds, an explicit opt-in, the core stays small | A config resource to validate; the store must remember kinds after config drops them |
| A model decides kinds at run time | No authoring | A restart can change answers with no new evidence (ADR 2) |

## Decision

**Vocabulary**

- Core kinds stay in code. Operators add kinds with `EntityKind`
  resources in the settings file, beside `Source`. Each gives the kind's
  name, an optional parent, its attributes (type, cardinality, conflict
  policy) and its relations (domain and range). The server builds one
  vocabulary from both at boot; config changes take a restart.
- A configured kind is `<namespace>.<Kind>`, matching
  `^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[A-Z][A-Za-z0-9]*$`, as in `k8s.Service` or
  `systemd.Service`. The namespace names the vocabulary that defines the
  kind, not a category; nothing reads meaning from it. Core kinds are bare
  (`Person`) and that is their only spelling: the `bearing` namespace is
  reserved and `bearing.Person` is rejected. A kind holds no `:` or `/`,
  so it can't be mistaken for a key (`issuer:type/value`), and attribute
  names hold no `.`.
- `extends` names one parent, core or configured. Chains end at the core
  or at nothing; cycles are rejected. A kind with no parent works with
  `Get`, `Related`, `Find` and `Changes` but never answers the five MVP
  questions. Questions about a kind include its descendants.
- An attribute or predicate name has one shape across the whole
  vocabulary (core, configured and adapter-declared): facts carry no kind,
  and cardinality decides which facts close. Boot rejects a config that
  gives a name a second shape.
- Configured kinds only grow. Boot compares config with the stored
  vocabulary and refuses a changed parent, a changed attribute shape or a
  narrowed domain or range; changing one means defining a new kind.
  Adapter-declared shapes are stored and checked the same way. A core
  change ships as a store migration, so it never trips the check.

**Adapters**

- A kind says what a thing is; provenance already says where it came from.
  One key type still has one kind: an adapter that can tell what a thing
  is (a Vault entity that is a person, or a group) uses a key type per
  kind. One that can't uses a key type of the parent kind, and the
  records join through `same_as` links once something tells them apart.
- Each declared kind may be marked `optional`; the default is required.
  When settings load, a required kind that is neither core nor configured
  rejects the adapter, naming the kind to enable. An optional one is
  accepted but inactive, and logged once. `DescribeResponse.emits` must
  name only declared kinds. Adapter bundles may ship a suggested config
  snippet; mappings are written by people and reviewed.
- `SyncRequest` and `HandleRequest` gain `enabled_kinds`: every active
  kind (core and configured) the adapter declares, from the vocabulary the
  server booted with. Empty means no filter, so an older core is read
  correctly. Adapters may use it to skip work; they never call Bearing for
  it, which keeps the calls one-way (ADR 4). It is a new field, with no
  protocol version change. A source with no active kinds is neither
  synced nor sent webhooks, and is logged once at boot, so an empty list
  never means "none".

**Ingest and the store**

- An observation whose kind is inactive (optional and not enabled, or
  retired) is dropped and counted per source and kind, never rejected:
  one invalid observation fails its whole page. Relations to inactive
  kinds are kept; their targets stay records without a kind until the
  kind is enabled.
- Closing ignores what Bearing didn't keep. The core intersects
  `CompleteSync.kinds` with the active kinds, and a fact is exempt from
  complete and snapshot closing when the record whose observation sent it
  has an inactive kind, so a disabled kind never reads as deleted. A
  relation an active record sends closes as usual, in either direction
  and whatever the other end's kind.
- At boot the vocabulary is written to a `kind` table. A kind dropped from
  config is marked retired, not deleted; re-adding it un-retires it.
  Reads resolve parents and shapes from the table, so stored facts of a
  retired kind still answer, under their parents too. Queries reject only
  kinds the table has never held.
- ADR 7's "never across kinds" becomes: every kind in a cluster lies on
  one line of descent. A link that would add a kind off that line is
  skipped.

## Consequences

- An operator can add a domain without a Bearing release, and two adapters
  in one domain share its kinds. An adapter upgrade that adds an optional
  kind runs at once; one that adds a required kind waits for the operator.
- `model.Kinds` and the package-level predicate map become a
  `*model.Registry` built at boot; validation, the store and queries take
  it. Tests build their own, so they stay parallel.
- Code that compares kinds exactly learns the hierarchy: the cluster check
  (`internal/duckstore/links.go`), complete closing and `Records`
  (descendants count), `match` candidate blocking (pairs along one line
  of descent), `Predicate.InDomain` and `InRange` (a descendant is in
  its parent's domain), and the matching half-life by kind (the nearest
  ancestor that has one).
- `proto/` gains `optional` on the declared kind and `enabled_kinds` on
  the two requests; `pkg/adapter` passes the list to adapters.
- Enabling a kind later takes a full sync from that source; webhook-only
  sources can't backfill (`docs/edge-cases.md`).
- ADR 7 is superseded in part when this is accepted.
