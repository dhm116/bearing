# 13. Backends store rows and one engine applies the rules

Date: 2026-10-08 · Status: proposed

## Context

The `GraphStore` contract ([contracts](../spec/contracts.md)) makes the store
do real work, not just keep rows. It canonicalizes subjects through merges,
resolves un-merge targets, replaces timelines while keeping the `recorded_at`
of unchanged rows, mints UUIDv7 IDs in increasing order, applies each event
once, and replays its change journal to restore a backup
([ADR 11](0011-identity-store-is-primary-state.md)). These rules interact,
and the data model changes them deliberately. A backend that writes them
again in its own language writes the same subtle behavior a second time.

[ADR 5](0005-one-store-to-start.md) makes SurrealDB the default backend, and
[ADR 2](0002-graph-is-source-of-truth.md) wants other backends to be
possible. The conformance suite
([`pkg/contracts/conformance`](../../pkg/contracts/conformance)) checks
each backend, but it can only find a divergence it already tests for.

Options considered:

| Option | Pros | Cons |
| --- | --- | --- |
| Each backend reimplements the rules, checked by conformance | Each can push work into its database; no engine in the process | The rules exist once per backend; a divergence ships until a test catches it; every new backend carries the whole burden |
| Rules in SurrealQL functions | Work stays inside the database | Only SurrealDB; the rules would be written a second time, in SurrealQL, where they are hard to test |
| One Go engine applies the rules; a backend loads rows into it and writes back the changes | The rules exist once; a bug is fixed once; a backend is rows, transactions and migrations | An operation loads the rows it needs into memory; the engine is production code, not only a test double |

## Decision

- **`internal/memstore` is the engine.** Besides serving `mem://`, its
  `Store` applies a `ChangeSet` and answers every read, and its
  `workingset.go` lets a backend load part of the state, run one operation
  and read back what changed. It is production code, held to the same
  review as the rest.
- **A backend stores rows.** It owns the row layout, migrations, the
  transaction that makes an apply atomic, concurrency and backup plumbing.
  It does not decide a rule.
- **SurrealDB works this way** (`internal/surrealstore`). Each operation
  loads the rows it needs, as a consistent snapshot at one head, into a
  scratch `memstore.Store`, runs the operation there, and writes the changes
  back in one transaction. The commit checks that the head is still the one
  the load saw and that the event is new. A lost race makes the apply reload
  and decide again; `ErrStale` reaches the caller when the `ChangeSet`'s
  `base_recorded_at` is behind the head the reload finds. An apply of more than 2,000 new rows stages them first in
  transactions of their own, invisible to readers because each row carries the
  record time it was written for and readers skip rows after the head. The
  commit moves the head and deletes rows left by attempts that never landed.
- **The conformance suite stays the check.** A backend passes it whatever it
  does inside. A third-party backend may use the engine or reimplement the
  rules; the contract does not change.
- **Supported credentials** are a database-scoped user (C-STORE-2).
  `?auth=root` is the development default until compose provisions that
  user.

## Consequences

- **What this protects.** A backend can differ from the reference only in
  how it stores rows. The data-model rules cannot drift between memstore and
  SurrealDB, and a rule bug is fixed in one place.
- **What it does not protect.** The storage layer is still each backend's
  own code, and it is where the SurrealDB review found its bugs, all fixed
  with regression tests: a name interpolated into `DEFINE USER`
  (`Provision`) that could inject statements; a staging transaction that could
  land after the commit that made its rows visible; and an un-merge with
  claims that wrote one series twice. Backend PRs therefore keep the
  `security` and `data` reviewers, and a change to `memstore` is a change to
  every backend's rules, so `data` reviews it too.
- **Cost.** An operation loads what it needs into memory, and the scratch
  store has no limits of its own. Every apply and every read loads the whole
  merge table, and reloads it on each conflict retry, which breaks
  C-STORE-9's rule that Apply depends only on the subjects it names. That is a documented exception in the threat
  model until [#81](https://github.com/dhm116/bearing/issues/81) is done.
  Meanwhile `surrealstore.DefaultMaxMerges` (20,000 merge records) turns
  unbounded work into `ErrTooManyMerges`. #81 also holds the way out: load
  only the merge components of the subjects an operation names, or keep a
  canonical-subject table written with each merge. The M3 benchmark
  ([#27](https://github.com/dhm116/bearing/issues/27)) measures it.
- **memstore's role changed.** Its package doc and the layout row in
  `AGENTS.md` say it is production code, `mem://` stays for tests and local
  trials, and C-STORE-10 records that its rule engine is trusted. Changes to
  it are reviewed that way.
- *Amended 2026-10-10:* `internal/surrealstore`, the backend this ADR
  describes in its Context and Cost, was removed by
  [ADR 14](0014-postgres-is-the-default-store.md). `internal/pgstore` follows
  the same design; its `component` table is the way out the Cost bullet
  names, and `DefaultMaxMerges` and `ErrTooManyMerges` went with
  `surrealstore`.
- **Supersedes in part ADR 5.** The graph is stored as rows, not as the
  `entity->fact->entity` edges ADR 5's Decision describes. ADR 5 carries the
  note.
- **Not decided here.** The shape of the contracts, `mem://` for tests, and
  ADR 5's choice of SurrealDB are unchanged. [ADR 14](0014-postgres-is-the-default-store.md)
  later replaces SurrealDB with PostgreSQL; the backend design here applies
  to `internal/pgstore` unchanged. Moving the engine out of
  `memstore` into a package named for what it does is open; it is a rename,
  not a design change, and can wait until a second backend needs it.
