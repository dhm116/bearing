# Component contracts

Bearing's core is a set of components that talk only through the interfaces
below. Each has one default backend; any other backend that passes the
interface's conformance suite can replace it. The Go definitions are in
[`pkg/contracts`](../../pkg/contracts/contracts.go).

| Interface | Responsibility | Default | Alternatives | Conformance suite |
| --- | --- | --- | --- | --- |
| `GraphStore` | Subjects, alias bindings, merges, supports, fact statuses, conflicts, data-quality issues and the resolver's state, bitemporally ([below](#graphstore)). The source of truth. | SurrealDB (`mem://` for tests) | PostgreSQL, Neo4j, Apache AGE, Memgraph | Yes (`conformance.GraphStore`) |
| `VectorIndex` | Semantic search over subjects and documents, keyed by subject ID | SurrealDB | Qdrant, pgvector, OpenSearch, Weaviate | Yes (`conformance.VectorIndex`) |
| `EventBus` | At-least-once delivery of CloudEvents between components | NATS JetStream | Kafka, SQS/SNS, Postgres queue | Planned |
| `Extractor` | Proposes candidate entities and relations from unstructured text | A self-hosted model behind a chat-completions style API (never a hosted LLM API by default) | Hosted models, only with a per-provider `insecure_hosted_model_<provider>` setting | Planned |
| `Judge` | Calibrated typed judgments: choice, yes/no, score | Kev 4B, self-hosted | Jev hosted API | Planned |
| `PolicyDecider` | Allow or deny an action, with the reason and how to fix it | Open Policy Agent | Cedar | Planned |
| `Authorizer` | Allow or deny a caller's request to Bearing by role, with the reason ([ADR 12](../adr/0012-authentication-through-oidc.md)) | OIDC group and client-ID to role mapping | OpenFGA, SpiceDB | Planned |
| `Executor` | Plan, apply, verify and roll back actions durably | Temporal | Postgres-backed job runner | Planned |

## One database to start

A backend can serve more than one interface. By default SurrealDB backs
both `GraphStore` and `VectorIndex`, so a small install runs one database, or
none with an embedded build ([ADR 5](../adr/0005-one-store-to-start.md)).
[`pkg/store`](../../pkg/store) opens backends from URLs:

| URL | Backend | Needs |
| --- | --- | --- |
| `mem://` | In-memory reference store | Nothing; data is lost on exit |
| `surrealdb+ws://user@host:8000?ns=bearing&db=main` | SurrealDB server (also `wss`, `http`, `https`) | A running `surreal start` |
| `surrealdb+mem://` | Embedded SurrealDB in memory | A `-tags surrealembed` build (CGO) |
| `surrealkv:///var/lib/bearing` | Embedded SurrealDB on disk | A `-tags surrealembed` build (CGO) |

`store.Config{Graph: url}` uses one backend for both. Setting
`Config.Vectors` to a second URL splits them, for example a SurrealDB graph
with Qdrant vectors, and nothing else changes. Passwords come from
`BEARING_STORE_PASSWORD`, not the URL; opening a URL that carries a password
MUST fail.

In a vector index, a point's kind is its `kind` payload field, which
`VectorQuery.Kinds` filters on. When subjects merge, the core calls
`Repoint` to move the merged subject's points to the survivor.

The SurrealDB backend serves both `GraphStore` and `VectorIndex`: a
SurrealDB store URL opens as either. It keeps the graph as rows and runs each
operation on the reference store's rules (see `internal/surrealstore`).

Beyond Go interfaces, components that run as separate services expose the
same operations over a network protocol (gRPC or HTTP+JSON), so a backend can
be written in any language. Those wire definitions will live in `proto/`.

## GraphStore

`GraphStore` holds the state of the [data model](data-model.md): the
identity store (subjects, alias bindings, merge and un-merge records) and
the claim store (supports, fact statuses, conflicts, data-quality
issues). The work is split in two:

- The **resolver** (issue #43) reads the store, applies the data model's
  rules to one event, and writes the outcome as a `ChangeSet`.
- The **store** applies a `ChangeSet` atomically and once per event ID,
  records it with a strictly increasing record time, and answers
  bitemporal reads. It checks only what it can check cheaply and without
  configuration.

### ChangeSet

A `ChangeSet` ([`changeset.proto`](../../proto/bearing/model/v1alpha1/changeset.proto))
is one event's writes:

| Field | Meaning |
| --- | --- |
| `event_id` | The event applied. The store applies each event ID once; a repeat writes nothing and returns the original result with `Duplicate` set. |
| `base_recorded_at` | The head (latest `recorded_at`) the resolver read at. If another apply has landed since, `Apply` fails with `ErrStale` and the resolver recomputes. |
| `recorded_at` | Set by the store. |
| `mints` | Subjects to mint, each with a ref `new:<label>` and a kind. |
| `bindings` | Alias binding timelines. |
| `supports` | Support timelines: one source's versions of its support for one fact, with subject and object as written. |
| `facts` | Fact status timelines: status, reason and confidence per valid-time span. Valid times no span covers have status `none`. |
| `conflicts` | Conflict timelines, one per (subject, predicate). |
| `issues` | Data-quality issue timelines, each under a key the resolver chooses. A key may name a subject the `ChangeSet` creates, by the [State keys](#state-keys) rule. |
| `merges` | Applied in order, each seeing the merges before it. Each needs a rule. |
| `unmerges` | Each un-merge's target is resolved against the state before the `ChangeSet`, not after the un-merges listed before it, and two un-merges may not claim the same merge record or subject. An un-merge has a ref (`new:<label>`, required, unique among the `ChangeSet`'s mints and un-merges) for the subject its aliases move to. |
| `state` | The resolver's own entries (ordering keys, watermarks, sync progress), as `google.protobuf.Any`. A key may name a subject the `ChangeSet` creates ([State keys](#state-keys)). |
| `audit` | What the resolver decided to audit in this event, in order ([Audit entries](#audit-entries)). |
| `merge_reviews` | Re-evaluations of merges recorded earlier, or in this `ChangeSet`: the per-side score and review status of a merge record ([Merge reviews](#merge-reviews)). |

Anywhere a subject ID appears in a `ChangeSet`, a ref may stand for a
subject the same `ChangeSet` creates; the store substitutes the minted ID.
A timeline item replaces its series' current timeline: rows equal to a
current row keep their `recorded_at`, other current rows are retracted at
this apply's record time, new rows are recorded at it, and an empty
timeline retracts the series. An alias, state key or issue key appears at
most once in a `ChangeSet`, judged after refs are substituted. A support's
`last_confirmed_at` is the exception to versioning: a confirmation updates
it in place, without a new version, so it is not bitemporal. Items apply in
this order: mints, un-merge targets, bindings, merges, un-merges, merge reviews, supports, facts, conflicts, issues, state.
Audit entries are not applied to anything; the store carries them.

A `ChangeSet` larger than `contracts.MaxChangeSetBytes` (16 MiB, by
`proto.Size`; a 5,000-fact `ChangeSet` with evidence on every support and a
state entry per fact is about 3.4 MiB, and the conformance suite keeps it
under a quarter of the limit) is refused, as is one that grows past it when recorded, so
every backup a store writes can be restored. So is one over a count limit,
which `contracts.CheckChangeSetLimits` checks (before the duplicate lookup)
so every backend refuses the same `ChangeSet`s:

| Limit | Value | Applies to |
| --- | --- | --- |
| `MaxChangeSetItems` | 50,000 | Entries in each of mints, bindings, supports, facts, conflicts, issues, state, audit entries and merge reviews |
| `MaxChangeSetMerges` | 250 | Merges, and separately un-merges |
| `MaxTimelineRows` | 256 | Rows in one binding, support, fact, conflict or issue timeline; aliases in one un-merge; positions of one conflict, objects of one position, subjects, aliases and supports of one issue; reviews on one merge record (a full record replaces its newest review) |
| `MaxAuditIDBytes`, `MaxAuditReasonBytes` | 1,024, 4,096 | Bytes in an audit entry's actor ID, target ID and rule, and in its reason |

The byte limit alone leaves the shape of a `ChangeSet` open: a timeline of
thousands of rows, or thousands of merges, costs a naive store time
quadratic in its size, and a merge reads the alias sets of its subjects. A
backend's `Apply` work MUST depend on the `ChangeSet` and the subjects and
aliases it names, not on the rest of the store: it matches rows by content
and finds a subject's current aliases without scanning every alias or
reading the history of aliases that moved away. At the limits the
reference store takes a second or two, whether or not its aliases were
rebound many times, and its tests fail at five seconds; a `ChangeSet` near
the byte limit takes a few seconds. A merge record carries both full alias
sets, so merges into a subject with many aliases are refused once their
records pass the byte limit; that amplification is accepted for now. A read
of aliases at an earlier record time is not a `ChangeSet` and has no such
bound: the reference store reads the history of every alias ever bound to
the subject. The values are low because raising a limit is compatible,
while lowering one can make an old backup unrestorable (`Restore` applies
every record under the limits in force) and makes a redelivered old event
fail instead of returning its original result. A merge has exactly two
subjects. Everything else, such as `Merge.evidence`, the aliases of a
subject and string lengths, is bounded by the byte limit alone.
A failed `Apply` writes nothing and returns a zero result. A repeated event ID returns the original
apply's result with `Duplicate` set, so a redelivery after a crash can
still re-point vectors.

The store keeps every applied `ChangeSet`, with the IDs it minted and what
it decided, in its change journal (not the audit log,
[ADR 8](../adr/0008-audit-log.md)).

Subject IDs are UUIDv7s with timestamps no later than the record time of
the apply that minted them. After a restore the store seeds its ID source
with the last restored ID, so if the clock is behind the restored data, new
IDs carry that ID's timestamp: IDs stay strictly increasing and can drift
from wall-clock time.

#### State keys

A state entry's key, and the key of an issue timeline, is opaque to the
store, with one exception: the store splits it at `/` and replaces every segment that is exactly a ref declared
in the `ChangeSet` (a mint's or an un-merge's) by the subject ID the ref
resolves to, as written: the minted ID, or for an un-merge the target, which
may be an existing subject whose entry the write then replaces. The store
does not canonicalize a subject in a key through merges. A segment that
starts with `new:` and isn't a declared ref makes the `ChangeSet` invalid,
as an unknown ref in a `subject_id` field does, and nothing is written. Two
keys that become one after substitution are refused as a repeat. So the
resolver can key what it remembers, or an issue it reports, about a subject
by the subject's ID in the event that creates it. A ref used in a key MUST NOT contain `/`. The
resolver MUST percent-encode `%`, `/` and `:` (as `%25`, `%2F` and `%3A`) in
any source-supplied text it puts in a key, so that only a subject segment
can be a ref and two different texts never share a key.

A state value is any `google.protobuf.Any`; the store keeps it as given and
never looks inside, so it can't replace a ref there. A value therefore never
names a subject the `ChangeSet` creates: the subject goes in the key. Because
an ID is longer than a ref, substituting keys can grow a `ChangeSet` past
`MaxChangeSetBytes`, which refuses it. The resolver's are the messages in
[`resolver/v1alpha1/state.proto`](../../proto/bearing/resolver/v1alpha1/state.proto),
which are not part of the data model. A backup replays state entries
verbatim, so those messages are part of the backup format: until the MVP
closes they change only by adding fields, never by renumbering or reusing
one, and they carry no version field. An entry is written in the same
`ChangeSet` as the rows it describes, so the two never disagree.

**Known limit.** The resolver's state entries grow with every
re-observation, because each is a new write with a greater ordering key and
the key decides what a late, older write does. Nothing prunes them yet.
Measured after 200 identical hourly syncs of one repository, one relation and
one attribute (about 200 bytes per sync for a binding or a scope's watermarks
and about 420 for a fact's support segments): a scope's watermarks (`wm/`) about 40 KB, an alias's binding
writes (`bind/`) about 40 KB, and a fact's support segments (`sup/`) 80 to 85
KB. An entry is rewritten whole by each sync and the store keeps every version
in the change journal, so storage grows quadratically per entry. The growth
also reaches the `ChangeSet`: it carries every entry its observation rewrites,
so a source that lists many facts approaches `MaxChangeSetBytes` and then has
every observation rejected as `too_large`. The `bearing.graph.state_entry.bytes`
metric shows the sizes. Tracked in [#77](https://github.com/dhm116/bearing/issues/77).

#### Audit entries

`ChangeSet.audit` holds what the resolver decided to audit in one event
([Audit](data-model.md#audit), [ADR 8](../adr/0008-audit-log.md)): a mint, a
binding written or released, a merge or un-merge, a status change an apply
caused, a conflict opened or closed, a rejection and the rest of that list.
Each entry is an `AuditEntry`
([`audit.proto`](../../proto/bearing/model/v1alpha1/audit.proto)): the action,
the actor and target (each a kind and an ID), the rule, the confidence, the
rejection code, the reason and the target's state before and after. The store
checks each entry's shape and refuses the `ChangeSet` if one fails:

- `action` is set and known;
- the actor ID, the target ID and the rule are at most `MaxAuditIDBytes`
  (1,024) and the reason at most `MaxAuditReasonBytes` (4,096), the same
  bounds a manual event's actor and reason have. The entries are kept for
  good and carry text from events and sources, so the bounds are settled
  before the first is stored. Control characters are not refused: whatever
  prints an entry's text MUST escape them;
- the actor and the target have a known kind and an ID;
- `rejection_code` is set when `action` is `rejection` and for no other
  action;
- a target of kind `subject` names a subject that exists, by ID or by a ref
  (`new:<label>`) for a subject the same `ChangeSet` creates, which the store
  replaces. No other ID is read, and `before` and `after` are kept as given.

The store keeps the entries as part of the `ChangeSet` in its change journal
and returns them in `ApplyResult.Audit`, in order and refs replaced, also for a
repeated event. It does not check that the entries cover the other items:
what to audit is the resolver's rule. The audit log adds the rest of a record
(its sequence number, the apply's record time and event ID, the trace ID and
the hash chain) when it writes the entries in the apply's transaction; that log
is a separate contract ([ADR 8](../adr/0008-audit-log.md)).

#### Merge reviews

A merge record is written once, but two things about it change after: the
per-side score of a `score` merge and whether its evidence still holds
([Merge](data-model.md#merge), step 4). The resolver reports them with
`ChangeSet.merge_reviews`; each `MergeReviewWrite` names a merge record by
the merged subject and the event that merged it (`subject_id`,
`merge_event_id`), which is unique, and carries a `MergeReview`: the
survivor's and the merged side's score in ppm (0 unless the rule is
`score`) and a status, `holds` or `needs_review`.

The store appends the review to the record's `reviews`, oldest first, with
the event and record time, and reads return the reviews recorded by the
read's record time, so the latest is current. A review equal to the latest
in score and status writes nothing, so a resolver that re-evaluates on
every apply does not grow the record. It refuses a `ChangeSet` that names a
merge that doesn't exist or names one merge twice, a missing or unknown
status and a score over 1,000,000. A record holds at most `MaxTimelineRows`
reviews: at the limit a new review replaces the newest, because a source can
flip its findings as often as it likes and the events that touch the merge
must not fail for it (the journal keeps the review replaced). A review may follow its merge in the same
`ChangeSet`, naming the merged subject by its ref. The store does not judge
the findings or open the conflict that `needs_review` goes with: that is the
resolver's.

#### Un-merge records

Every un-merge leaves an `UnmergeRecord`, which `Unmerges(subject, t)`
returns for the subject the aliases left and for the one they moved to, oldest
first, as recorded at `t`. It holds both subjects, the sorted aliases that
moved, whether the target is a split mint, the event and record time and, for
a reactivated subject, the event that had merged it. A reactivation also
closes its merge record (`unmerged_at`, which `Merges` returns); a split
mints a subject and writes no merge record, and so, until this record, left
nothing that said where its subject came from.

**Un-merging through a chain.** If B merged into A and A then into Z,
un-merging B's aliases from Z finds no merge of Z with exactly that alias
set, so it mints a split and B stays merged into A. That is the rule as
written ([Un-merge](data-model.md#un-merge), step 1). To restore B exactly,
un-merge the later merge first (A's aliases at the time, B's included, from
Z) and then B's aliases from A. `bearing get` says so beside the merges it
lists.

### Backup

`Backup` writes a stream of length-delimited `BackupFrame` messages
([`backup.proto`](../../proto/bearing/model/v1alpha1/backup.proto)), read
and written with `contracts.BackupReader` and `contracts.BackupWriter`:

1. A header: the body's format name and version, the store's head, the
   last subject ID it minted and `taken_at`, the store's clock when it wrote
   the backup (or its head, if the clock had stepped back behind it).
2. The body: records in the format the header names, which the store
   defines. No record frame is larger than `MaxChangeSetBytes`.
3. A trailer: the number of records and a SHA-256 of every byte before it.

`Restore` works only into an empty store. It MUST refuse a format or
version it doesn't know, a header without `taken_at`, a record later than
`taken_at`, a stream without its trailer (one truncated, even at
a frame boundary), a bad checksum or record count, and data after the
trailer, and MUST leave the store empty on any failure. It MUST NOT
refuse a record for being later than the restoring store's own clock,
which may be behind the host that wrote the backup; the bound is `taken_at`.
A restore therefore leaves the head, the record times and the ID
timestamps at the backup's values, even if they are ahead of the host's
clock; the next applies are recorded just after the head, and the store
does not judge whether `taken_at` is plausible. After a restore,
every read at every record time up to the head answers as it did in the
original store, processed events stay processed, and the head and the order
of subject IDs are kept.

The reference store's body is its change journal, one `JournalEntry` per
record: each `ChangeSet` as applied, with the merge records, un-merge
targets (reactivated or split) and mints it decided. Restore replays the
journal and fails if any apply decides differently, so a change of rules
can't silently re-decide history. It also refuses an entry with no
`recorded_at` or one later than the header's `taken_at`.

### Who owns which rule

References are to sections of the [data model](data-model.md).

| Rule | Owner |
| --- | --- |
| Apply and isolation: one transaction per event, the processed-event mark, `recorded_at = max(now, previous + 1 µs)`, serialized applies | Store. Applies are serialized by compare-and-swap on the head (`ErrStale`). |
| Idempotency: re-applying a processed event is a no-op | Store |
| Subjects: minting UUIDv7 IDs in increasing order, never reused, also after a restore | Store |
| Minting: when to mint and with which rule | Resolver |
| Bindings: what each binding write maps, `per_subject`, back-extension, tentative bindings, redirects | Resolver, which writes the resulting timeline |
| Resolution: looking up an alias at a valid and record time, following merges | Store (`ResolveKey`, `Bindings`) |
| Resolution rules 1–3, rejections such as `kind_mismatch` | Resolver |
| Case folding of `insensitive` keys: `ResolveKey`, `Bindings` and the filters match aliases exactly as written | Resolver, which writes and looks up the folded form |
| Once any alias of a key type is bound, its kind, class (`id` or `name`) and case sensitivity are fixed | Configuration apply, which reads the bindings and rejects the change. The store doesn't know key types. |
| Audit entries: their shape and subjects, carried with the `ChangeSet` ([Audit entries](#audit-entries)) | Store. The audit log writes the records ([ADR 8](../adr/0008-audit-log.md)). |
| Count limits on a `ChangeSet` ([GraphStore](#graphstore)) | Store, through `contracts.CheckChangeSetLimits`. The resolver keeps every series under the row limit (merging adjacent equal spans, compacting) and splits a larger write across events where it can; a write it can't split, such as a snapshot scope too big for one `ChangeSet`, it rejects with an audit entry rather than retrying (`too_large`). Compacting and splitting are not implemented yet: the resolver merges adjacent equal spans and rejects anything still over a limit. |
| Merge: survivor is the lower ID, `status`/`merged_into`, reads canonicalize from `r`, earlier reads show two subjects, alias sets on the record. An alias set holds every alias with a row mapping it to the subject as recorded at the merge, released rows that redirect and tentative rows included. | Store |
| Merge review: re-evaluating a merge's evidence, the per-side score and the status, and opening the conflict for review | Resolver. The store appends the review to the merge record ([Merge reviews](#merge-reviews)). |
| Merge: same kind, both active | Store checks; resolver decides |
| Merge triggers, policies, the guard, evidence re-evaluation | Resolver |
| Un-merge: reactivating the subject whose alias set matches, else a `split` mint; no un-merge of a `placeholder` merge; aliases a non-empty proper subset; the un-merge record ([Un-merge records](#un-merge-records)) | Store |
| Un-merge: re-pointing bindings and claims, setting `distinct_from` | Resolver. It re-points by the binding rows as written, not by alias membership. |
| Backup and restore of primary state (ADR 11) | Store ([Backup](#backup)) |
| Vector points re-pointed on merge | Core, through `VectorIndex.Repoint` |
| Supports and fact statuses as timelines; reads that canonicalize subject, object and `fact_id` through merges; `last_confirmed_at` updated in place without a new version | Store (`Supports`, `AsOf`, `Changes`) |
| Claims, ordering and idempotency of claims, snapshot scopes, sync completeness, derived claims, confidence, status, matching, manual operations, which events to audit and what each entry says | Resolver, which writes the resulting support and fact timelines. The ordering keys of writes, which the support rows don't carry, are the resolver's state ([State keys](#state-keys)). |
| Conflicts and data-quality issues as timelines, canonicalized on read | Store (`Conflicts`, `DataQuality`) |
| Detecting conflicts and data-quality issues, and when they end | Resolver, which writes the resulting timelines |

### Reads

Every read takes a record time and returns what was recorded at or before
it (`Merges` also leaves out the reviews and the un-merge recorded after it); `ResolveKey`, `AsOf`, `Conflicts` and `DataQuality` also take a valid
time, and `Changes` compares two points on the valid or the record axis. A
zero time means now, for either point of `Changes` too. Subject IDs in
answers are canonical as of the record time, except where a method returns rows as written (`Bindings`,
`Supports`, and `Merges` and `Unmerges`, whose records name the subjects
they involve). The resolver reads the head with `Head`, reads at that record
time, and sets `base_recorded_at` to it, so its reads are a consistent
snapshot.

Reads of facts are deterministic, so the same state gives the same answer
every time and in every backend:

- `AsOf` and `Changes` return facts ordered by canonical subject ID,
  predicate, then fact ID. A valid time that no span covers has no row, even
  for a `none` filter; only an explicit `none` span is returned.
- `Supports` returns timelines ordered by canonical fact ID, then source,
  then the fact ID as written. Each timeline's versions are ordered by
  `valid_from` (unbounded first), then `valid_to` (unbounded last).
- A fact's supports in `AsOf` are ordered by source, then `valid_from`.
- Timelines written under different subjects or objects can canonicalize to
  one fact after a merge. If more than one has a span at the point, the span
  with the highest `confidence_ppm` wins; ties go to the greater `status`
  (enum order), then the greater `status_reason`, then the earlier
  `valid_from` (unbounded first), then the earlier `valid_to` (unbounded
  last). If the spans are equal in all of these, the timeline whose fact ID
  as written is smaller wins. The answer's interval is the winning
  timeline's maximal run of spans with that status; runs from different
  timelines are not joined.
- A `FactFilter` whose `Object` is not a valid fact object, and a `Changes`
  call with an unknown axis, are errors, never a filter that matches
  everything.
- `Conflicts` returns the conflicts covering the valid time, ordered by
  canonical subject ID, predicate, then the key of the timeline as written
  (subject ID as written). An empty subject or predicate matches anything;
  a subject is canonicalized before it is matched, so a merged-away ID
  finds the survivor's conflicts. A conflict stays queryable after its
  `resolution` is set. Timelines written under different subjects can
  canonicalize to one (subject, predicate) after a merge; each still
  answers, so the resolver MUST retract or rewrite the merged-away
  subject's timeline in the `ChangeSet` that records the merge, or a caller
  sees the stale conflict beside the new one. Objects in a position that a
  merge makes equal are named once, keeping the first.
- `DataQuality` returns the issues covering the valid time, ordered by the
  key of their timeline. Subject IDs are canonical, and a subject that
  several merged subjects canonicalize to is named once. An `IssueFilter`
  with an unspecified or unknown issue type is an error. Its fields combine
  with AND, and a value within a field with OR; an empty field matches
  anything. `Types` matches issues of one of those types, `Kinds` an issue
  naming a subject of one of those kinds, `Sources` one with a support from
  one of those sources.
- A conflict or issue span covers the half-open interval from `valid_from`
  to `valid_to`, as every timeline row does.
- What `Apply` accepts: a conflict timeline needs a subject and a predicate,
  none twice in a `ChangeSet`; each conflict in it is for that subject and
  predicate, with at least one position, each with a `source_system` and
  valid objects. An issue timeline needs a key, none twice; each span needs
  a known issue type, and each support in it a source. Nothing else in a
  conflict or issue is validated: a position's `authority` and a support's
  `confidence_ppm` are kept as written.

## Rules that apply to every component

- **The graph is the source of truth.** The vector index, caches and search
  are derived from it and can be rebuilt from it. Policy and ownership
  answers read only asserted graph facts.
- **Candidates are not facts.** Anything an `Extractor` proposes goes through
  a `Judge` before it reaches the graph.
- **No hosted LLM by default.** Source text is untrusted and often private,
  so no default component, including the default `Extractor`, MUST send it
  to a hosted LLM or embedding API. An operator enables a hosted provider
  explicitly, per provider, with its config listing the fields sent (threat
  model C-MODEL-1, C-MODEL-2).
- **Idempotent consumers.** Event delivery is at least once. Handlers must
  tolerate duplicates.
- **Least privilege.** Components that read external systems (adapters) hold
  read-only credentials. Only the `Executor` holds write credentials, and
  only after a `PolicyDecider` allows the action.

## Adding a backend

1. Implement the interface.
2. Call the conformance suite from your backend's tests:

   ```go
   func TestConformance(t *testing.T) {
       conformance.GraphStore(t, func(t *testing.T) (contracts.GraphStore, conformance.Clock, conformance.IDs) {
           clk := testkit.NewClock(time.Time{})
           ids := testkit.NewUUIDv7s(clk.Now)
           return newEmptyStoreForTest(t, clk.Now, ids), clk, ids
       })
   }
   ```

   The suite drives the store's clock, and reads the last ID its ID
   source issued; `testkit.FakeClock` satisfies `conformance.Clock` and
   `model.UUIDv7Source` satisfies `conformance.IDs`.

3. Document any behavior the suite doesn't cover (consistency, limits).

[`internal/memstore`](../../internal/memstore) is the reference `GraphStore`
and `VectorIndex` and shows the pattern.
[`internal/surrealstore`](../../internal/surrealstore) passes the
`VectorIndex` and `GraphStore` suites.
