# Component contracts

Bearing's core is a set of components that talk only through the interfaces
below. Each has one default backend; any other backend that passes the
interface's conformance suite can replace it. The Go definitions are in
[`pkg/contracts`](../../pkg/contracts/contracts.go).

| Interface | Responsibility | Default | Alternatives | Conformance suite |
| --- | --- | --- | --- | --- |
| `GraphStore` | Subjects, alias bindings, merges, supports, fact statuses, conflicts, data-quality issues and the resolver's state, bitemporally ([below](#graphstore)). The source of truth. | PostgreSQL (`mem://` for tests) | Neo4j, Apache AGE, Memgraph | Yes (`conformance.GraphStore`) |
| `VectorIndex` | Semantic search over subjects and documents, keyed by subject ID | PostgreSQL with pgvector | Qdrant, OpenSearch, Weaviate | Yes (`conformance.VectorIndex`) |
| `EventLog` | The durable, ordered, replayable log every input enters through, with consumer offsets and a retention window ([below](#eventlog)) | PostgreSQL (`mem://` for tests) | NATS JetStream, Kafka | Yes (`conformance.EventLog`) |
| `AuditLog` | The tamper-evident record of who or what changed a fact and why: a hash chain written in the same transaction as the change, checkpoints kept outside the store ([below](#auditlog)) | The graph's own backend, PostgreSQL (`mem://` for tests) | Any backend that serves `GraphStore` | Yes (`conformance.AuditLog`) |
| `Extractor` | Proposes candidate entities and relations from unstructured text | A self-hosted model behind a chat-completions style API (never a hosted LLM API by default) | Hosted models, only with a per-provider `insecure_hosted_model_<provider>` setting | Planned |
| `Judge` | Calibrated typed judgments: choice, yes/no, score | Kev 4B, self-hosted | Jev hosted API | Planned |
| `PolicyDecider` | Allow or deny an action, with the reason and how to fix it | Open Policy Agent | Cedar | Planned |
| `Authorizer` | Allow or deny a caller's request to Bearing by role, with the reason ([ADR 12](../adr/0012-authentication-through-oidc.md)) | OIDC group and client-ID to role mapping | OpenFGA, SpiceDB | Planned |
| `Executor` | Plan, apply, verify and roll back actions durably | Temporal | Postgres-backed job runner | Planned |

## One database to start

A backend can serve more than one interface. By default PostgreSQL backs
`GraphStore`, `VectorIndex` (the vectors use the pgvector extension),
`EventLog` and the `AuditLog` that `GraphStore` writes, so a small install runs one database ([ADR 14](../adr/0014-postgres-is-the-default-store.md)).
[`pkg/store`](../../pkg/store) opens backends from URLs:

| URL | Backend | Needs |
| --- | --- | --- |
| `mem://` | In-memory reference store | Nothing; data is lost on exit |
| `postgres://user@host/db?vector_dimensions=384` | PostgreSQL server (`GraphStore`, `EventLog` and the `AuditLog`; also `VectorIndex` when `vector_dimensions` is set) | PostgreSQL 16 or later, and pgvector 0.5 or later for vectors |

`store.Config{Graph: url}` uses one backend for all three. Setting
`Config.Vectors` or `Config.Events` to a second URL splits them, for example a
PostgreSQL graph with Qdrant vectors, and nothing else changes. The event log
shares the graph's database but none of its tables, and `Backup` and `Restore`
do not touch it. Passwords come from
`BEARING_STORE_PASSWORD`, not the URL; opening a URL that carries a password
MUST fail.

In a vector index, a point's kind is its `kind` payload field, which
`VectorQuery.Kinds` filters on. When subjects merge, the core calls
`Repoint` to move the merged subject's points to the survivor.

The PostgreSQL backend serves `GraphStore`, and `VectorIndex` too when the
URL sets `vector_dimensions`. It keeps the graph as rows and runs each
operation on the reference store's rules (see `internal/pgstore`).

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
| `trace_id` | The W3C trace ID of the work that produced the `ChangeSet` (32 lowercase hex digits, not all zero), or empty. The store refuses any other value. The audit log copies it into each record, and the journal keeps it so a restore rebuilds the same records ([AuditLog](#auditlog)). |

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

**Growth.** The resolver's state entries stay as small as the changes a source
reports, not the syncs it makes: a re-observation that says what an entry
already says moves the entry's ordering key to its own and adds no record
([Confirmations](data-model.md#confirmations)). A `sup/` entry (a fact's
support segments) and a `bind/` entry (an alias's binding writes) are about 400
bytes however many times a source syncs. The one exception is the `wm/` entry
of a snapshot scope, which gets a watermark per sync (about 200 bytes) until
[#77](https://github.com/dhm116/bearing/issues/77) bounds it. An entry is
rewritten whole by each sync and the store keeps every version in the change
journal, so the `wm/` entry's storage grows quadratically, and a `ChangeSet`
that carries it reaches `MaxChangeSetBytes` only after tens of thousands of
syncs. The `bearing.graph.state_entry.bytes` metric shows the sizes.

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
- `confidence_ppm` is at most 1,000,000;
- the actor and the target have a known kind and an ID;
- `rejection_code` is set when `action` is `rejection` and for no other
  action;
- a subject ID in an entry is the ID of a subject that exists, or a ref
  (`new:<label>`) for a subject the same `ChangeSet` creates, which the store
  replaces. Subject IDs are read in three places:
  - the ID of a target of kind `subject`;
  - the part of a target of kind `subject_predicate` before its first `/`.
    The target needs both parts, and a ref used there MUST NOT contain `/`
    (the rule for [State keys](#state-keys)), or it names no mint;
  - the `subject_id`, `subject_ids`, `survivor_id`, `merged_id`,
    `merged_into` and `target_id` fields of the messages in `before` and
    `after`, at any depth: the store walks nested and repeated messages (a
    `BindingTimeline` holds its `subject_id` in `bindings`, a `FactTimeline`
    in `object`, an `UnmergeRecord` its `target_id`). It does not read map
    values or fields of `google.protobuf` types, and it refuses a nested
    `Any`, which it could not read.

  The store checks each one, and refuses the `ChangeSet` for a ref no mint or
  un-merge declares (so no subject ID leaves the store as a ref) and for a
  subject that doesn't exist (`ErrNotFound`). It refuses an embedded message
  that is malformed or not a message of the `bearing.model.v1alpha1` or
  `bearing.config.v1alpha1` packages (a configuration change embeds a
  `Resource`), since a ref inside it could not be found. Where it replaces a
  ref inside `before` or `after` it re-encodes the message with the Go
  Protobuf library's deterministic marshalling and keeps the `type_url` the
  `Any` came with; a message with no ref is kept byte for byte. An empty
  string in a `subject_ids` list is not checked (it is for the entry's writer
  to explain), and the fields above are matched by name, so a field of those
  names in a message of either package holds a subject ID. Nothing needs the bytes to match across builds or libraries,
  because the audit log hashes the bytes the store hands it. The other
  targets (`alias`, `fact`, `source`, `resource`, `event`) hold no subject ID
  and are kept as given: a fact's ID is a hash, so the status change of a
  fact of a subject minted in the same event is audited with a
  `subject_predicate` target and its `FactTimeline` in `after`. Entries are
  history and are not re-pointed when subjects merge later.

The store keeps the entries as part of the `ChangeSet` in its change journal
and returns them in `ApplyResult.Audit`, in order and refs replaced, also for a
repeated event. It does not check that the entries cover the other items:
what to audit is the resolver's rule. The audit log adds the rest of a record
(its sequence number, the apply's record time and event ID, the trace ID and
the hash chain) when it writes the entries in the apply's transaction
([AuditLog](#auditlog)).

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
must not fail for it (the journal keeps the review replaced; a read as recorded between the
replaced review and the one that replaced it does not give the answer the
store gave then). A review may follow its merge in the same
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

## EventLog

`EventLog` is the log of [ADR 7](../adr/0007-durable-event-log.md): webhook
deliveries, sync requests, adapter output, people's operations and the
core's own events all become events on it, and the workers that run adapters
and apply their output read from it. It replaces the `EventBus` of earlier
drafts. A webhook is acknowledged only after `Append` returns for its event,
and a failing log makes the ingest path answer 5xx so the sender retries
([C-INGEST-8](../security/threat-model.md)).

An `Event` carries these CloudEvents attributes and a payload:

| Field | Rule |
| --- | --- |
| `ID` | Stable for the same input, so a redelivery repeats it: `<partition>/<local ID>`, where the local ID is the source's delivery ID or a content hash and holds no slash (a delivery ID with a slash is hashed). The partition is everything before the last slash, so an ID names its partition and two partitions can never produce one ID, even when source names hold a slash. At most 512 bytes. [Event IDs](#event-ids) says how each event gets one. |
| `Partition` | The unit of order ([below](#partitions)). |
| `Type` | The CloudEvents type, `dev.bearing.<message in snake case>.v1` ([Event types](#event-types)). |
| `Time` | When the event happened, UTC and truncated to microseconds. Required. |
| `Data` | The payload message in ProtoJSON. Required, at most 8 MiB. The log stores it and does not read it. |
| `Retain` | Exempts the event from `Trim` until `Release` ([Retention](#retention)). Required on every event in the `manual` partition. |

The CloudEvents `specversion` is `1.0` and `datacontenttype` `application/json`
for every event; the log keeps neither. A partition, a group and a type are
1 to 256 bytes of valid UTF-8 without control characters.

### Partitions

Events are ordered within a partition and not across partitions. A
partition is the configured source an event is about, so the events of one
source, its syncs, its webhook deliveries and its adapter output, are read
in the order they were appended, which is what identity decisions need
([data model](data-model.md#state-determinism-and-apply)). Two reserved
names cover events that belong to no source, because the names `manual` and
`core/...` can't be source names:

- `manual`: people's and agents' operations.
- `core/<name>`: events the core appends about itself, one partition per
  component.

An offset is the event's position in its partition: positive, and strictly
increasing in the order events were appended. It is not promised to be
consecutive, because `Trim` leaves gaps and a backend may skip numbers (Kafka
does), and offsets are not comparable across partitions.

### Operations

| Method | What it does |
| --- | --- |
| `Append(events)` | Writes the events atomically and in order, and returns each one's partition and offset. Events of one partition get increasing offsets in the order given. It returns only after the events are durable as the backend promises (`mem://` promises nothing past the process). A repeated ID writes nothing and returns the original position with `Duplicate` set; so does an ID repeated within one call. At most 1,000 events and 32 MiB per call. |
| `Read(partition, after, limit)` | Returns the entries with an offset greater than `after`, oldest first: at most `limit` (1 to 1,000) and at most 32 MiB of data, but at least one if any qualify. It doesn't wait for new events; callers poll. An entry never becomes visible before the ones with lower offsets in its partition. A bad partition, offset or limit is `ErrInvalidRequest`. |
| `Commit(group, partition, offset)` | Records that the group has processed the partition up to `offset`. A commit never moves a group back: a lower or equal offset changes nothing. A bad name or negative offset is `ErrInvalidRequest`; then an unknown partition is `ErrNotFound`; then an offset beyond the partition's head is `ErrInvalidRequest`. |
| `Committed(group, partition)` | The group's offset, or 0. A bad name is `ErrInvalidRequest`. |
| `Partitions()` | Every partition with its `Head` (latest offset) and `Trimmed` (highest offset `Trim` removed). A partition whose entries were all trimmed is still listed, with its `Head` intact, and offsets are never reused. |
| `Trim(before, groups)` | Removes the entries appended before `before` that aren't retained and that every group in `groups` has processed (its committed offset in the partition, 0 if none, is at or above the entry's), and returns the count. A zero `before`, an empty `groups`, a bad group name or more than 64 groups (duplicates count) is `ErrInvalidRequest`. See [Retention](#retention). |
| `Release(ids)` | Clears `Retain` on those events. Unknown IDs and an empty list are ignored; more than 1,000 IDs or an ID out of bounds is `ErrInvalidEvent`. |

`Append` fails with `ErrInvalidEvent` when the batch or an event breaks the
rules above, which `contracts.CheckEvents` checks so every backend refuses
the same events, and writes none of the batch. `AppendedAt`, the time the log
took an entry, comes from the backend's injectable clock, not from the
database's transaction time, and `Trim` tolerates a clock that steps back.
`Data` is stored byte for byte.

A consumer reads a partition after the offset its group committed,
processes the entries, and commits the last offset it finished. Delivery is
at least once, and `GraphStore.Apply`'s processed-event mark makes a repeat
harmless. The log doesn't assign partitions to consumers or lock them: two
consumers of one group on one partition would process out of order, so the
server runs one per partition and group, and until there is a lease that
means one server instance (issue #139 settles it for replicas). A new group
has no offset and starts at the oldest retained entry. A group that has
committed before and sits below `Trimmed` has lost events and reports that
instead of reading on. Different groups read the same events independently.

Duplicates are found for as long as the original is retained. After `Trim`
removes it, the same ID appends again as a new event; the processed-event
mark is what stops a second apply. The log doesn't compare the content of
two events with one ID, since the ID is meant to name the content.

Offsets are assigned in append order and never move, but a backend need not
number them densely, and `Read` takes any `after`, so a consumer never
computes an offset itself. A backend serializes appends to one partition
until they commit (on PostgreSQL, a lock on the partition's head row, taken
in partition order when a call spans partitions), which is what keeps a
lower offset from becoming visible after a higher one; sequence gaps from
rolled-back appends are then harmless.

### Retention

`Trim` is the event log's retention window: the data model's "event log
retention" ([State, determinism and apply](data-model.md#state-determinism-and-apply))
is the `before` the caller passes, now minus the configured window (default
30 days, ADR 7). It goes by the time the log appended the entry (`AppendedAt`),
not the event's `Time`.

The window counts applied events. `Trim` takes the **required groups**, the
consumer groups whose work must not be lost (the server passes the groups
that apply events to the graph), and keeps an entry until every one of them
has processed it: an entry goes only when each required group's committed
offset in that partition is at or above the entry's offset. A required group
that has never committed in a partition holds back everything there, since
nothing was applied, so a required group is expected to read every partition.
A group not named holds nothing back, so a stray reader (a debugging tool, an
old experiment) cannot pin the log. `Trim` refuses an empty list, so a
configuration slip cannot turn the window back into deletion by age alone.
This is what makes the at-least-once delivery above hold after a long outage:
an event the log acknowledged to a sender is not deleted before it was
applied. The cost is that a consumer that stops makes the log grow until it
recovers; keeping that visible and bounded (a gauge for the age of the oldest
unapplied event, a dead-letter record for events a worker cannot apply)
belongs to the server, not to the log. A group that is not required and lags
behind the window loses events, which `Partitions` shows as `Trimmed` above
its offset.

Manual events are exempt for as long as their effects are live
([ADR 11](../adr/0011-identity-store-is-primary-state.md)). The log refuses
one in the `manual` partition without `Retain`, since a missing flag would
silently lose primary state at the window. The core calls `Release` when
an event's effects end, for example when an override is cleared or
compacted away, and only after the write that ends them has committed: a
crash in between leaves the event retained, which is harmless.

A backend whose store can only drop a prefix of a partition (Kafka) or that
has no index by ID (JetStream) keeps retained events in a side store to
implement `Trim` and `Release`; the contract is what the caller sees.

### Event types

Each event message in `proto/bearing/event/v1alpha1` has a CloudEvents type
(`model.EventType`) and a partition. Parts of events (`Actor`, `Header`,
`ConfigChange`) are not events.

| Message | CloudEvents type | Partition |
| --- | --- | --- |
| `SyncRequested`, `ObservationsEmitted`, `WebhookReceived` | `dev.bearing.sync_requested.v1`, `dev.bearing.observations_emitted.v1`, `dev.bearing.webhook_received.v1` | The event's `source` |
| `DeclarationChanged`, `SubjectDeletionDerived` | `dev.bearing.declaration_changed.v1`, `dev.bearing.subject_deletion_derived.v1` | The event's `source`, so each is read after the syncs it follows |
| `MergeRequested`, `UnmergeRequested`, `DistinctFromSet`, `DistinctFromCleared`, `ClaimWithdrawn`, `OverrideSet`, `OverrideCleared` | `dev.bearing.<name>.v1` | `manual`, appended with `Retain` |
| `ValidTimeBoundaryReached`, `CompactionRequested` | `dev.bearing.<name>.v1` | `core/scheduler`, also when a person requests a compaction |
| `ConflictOpened`, `ConflictResolved`, `OverrideStale` | `dev.bearing.<name>.v1` | `core/resolver` |
| `ConfigApplied` | `dev.bearing.config_applied.v1` | `core/config` |

`Observation` is the CloudEvent of one observation, as an adapter emits it
([Observations](data-model.md#observations)); the log carries it inside
`ObservationsEmitted` and never as an event of its own, so it has no row
here and its type is `model.ObservationType`.

#### Event IDs

The local part of an ID depends on where the event comes from:

- A delivery from a source: the source's delivery ID, else the lower-case
  hex SHA-256 of the authenticated body (C-INGEST-5). A delivery ID that
  holds a slash is replaced by the hex SHA-256 of its bytes.
- A request from a person, the scheduler or the CLI (`SyncRequested`,
  `CompactionRequested`, `ConfigApplied` and the manual operations): a fresh
  UUID, not a hash, so two identical requests are two events.
- Adapter output: derived from the event it answers and the page number, so
  a replayed run repeats its IDs. A derived local ID is the lower-case hex
  SHA-256 of the causing event's ID, a NUL byte and a discriminator (the
  page number), because the causing ID holds a slash.
- Events the core reports after an apply (`ConflictOpened`,
  `ConflictResolved`, `OverrideStale`, `SubjectDeletionDerived`,
  `ValidTimeBoundaryReached`): derived from the event that caused them and
  what they report, by the same hash, so the same replay appends the same
  events and they dedupe. Whether the core appends them before or after the apply commits is
  the server's design (issue #139); the IDs make either order safe.

## AuditLog

`AuditLog` is the record of who or what changed a fact and why, which the
change journal is not: the journal keeps every write of a `ChangeSet`, the
audit log keeps the decisions people rely on, and it can show that none was
edited, removed or cut off ([ADR 8](../adr/0008-audit-log.md), threat model
C-AUDIT-1 to C-AUDIT-6). The Go definition is
[`pkg/contracts/audit.go`](../../pkg/contracts/audit.go); the bytes that are
hashed and the verifier are in [`pkg/audit`](../../pkg/audit).

The interface only reads. The backend that serves `GraphStore` writes the
records itself, inside the transaction of the `Apply` that carries their
entries, so no change commits without its record and no record exists for a
change that did not commit. There is no separate append call: a record
written apart from its change could be lost or orphaned. (Writers that change
no graph state, such as policy decisions and executor actions in ADR 8, get
an append when the first of them exists.) Nothing updates or deletes a
record. Because the log lives in the graph's backend, `store.Store.Audit` is
that backend; it has no URL of its own. A backend whose `GraphStore` already
has a `Head` method offers the log through an `AuditLog()` method, since the
two `Head`s differ.

### Records

For each entry of an applied `ChangeSet.audit`, in order, `Apply` writes one
`AuditRecord`
([`audit.proto`](../../proto/bearing/model/v1alpha1/audit.proto)):

| Field | Value |
| --- | --- |
| `seq` | The previous record's `seq` plus one; 1 for the first. The numbers have no gaps: an `Apply` that fails uses none. |
| `recorded_at` | The apply's record time, a whole number of microseconds (as every record time is), so a backend that stores microseconds reads back what was hashed. `audit.Seal` refuses any other. |
| `event_id`, `ordinal` | The event, and the entry's position among the event's entries from 1. They are unique together. |
| `trace_id` | `ChangeSet.trace_id`, which the journal keeps; empty when there was none. |
| `entry` | The entry as `Apply` returns it in `ApplyResult.Audit`, refs replaced. |
| `prev_hash` | The previous record's `hash`; empty for the first record. |
| `hash` | See below. |

An event that audits nothing writes nothing, and a repeated event writes
nothing. Everything in a record comes from the journal entry (the
`ChangeSet` as applied, with its trace ID), so `Restore`, which replays the
journal, rebuilds the same records with the same hashes: a restored store
continues the chain, and checkpoints written before the backup still match
it. Records after the backup's head are lost with the rest of the store's
tail, and a checkpoint beyond the restored head says so
([Verify](#verify)).

### The chain

`hash` is the SHA-256 of the bytes `"bearing.audit.record.v1"`, a NUL, and
the **canonical encoding** of the record without its `hash` field. The
canonical encoding is the Protobuf wire encoding with the fields of each
message in ascending field number, a field at its default value left out, a
message that is present but empty written as an empty message, and `before`
and `after` written as their `type_url` and `value` bytes exactly as stored.
Messages with unknown fields, repeated fields and maps can't be encoded; a
record that has one fails verification. Leaving out defaults means a field
added to the record later does not change the hash of a record written
without it, and a field that is set is always covered: `pkg/audit` tests that
changing any field of a record changes its hash. A change to these rules
takes a new version in the leading bytes, never an edit of version 1.
`audit.Hash` is the reference; its test pins the hash of a known record.
A verifier fails closed on a field it does not know, so it must be at least
as new as the store that wrote the log, and a repeated or map field in a
record would need its own encoding rule first. A record is no larger than the
`ChangeSet` that carried its entry ([`MaxChangeSetBytes`](#graphstore)),
which bounds `before` and `after`.

The chain alone shows that the records are consistent with each other. Whoever
can write to the store can rewrite every record from the one they edit and
recompute the hashes, and the result verifies. Checkpoints close that gap.

### Checkpoints

A checkpoint is an `AuditCheckpoint`: the `seq` and `hash` of a record, the
time it was written, and, when a signing key is configured, the key's ID and
a signature. The core writes one at an interval and at every retention cut
(the server schedules them, issue #139; `bearing audit checkpoint` writes one
now). It writes them **outside the store**: to the log stream, a file or an
exporter, never to a table the store's writers can edit. The operator keeps
that place where the store's writers can't write. A checkpoint is one line of
ProtoJSON, so a file of them is NDJSON.

What the checkpoints protect depends on where they go. The server process
writes the records, holds the signing key and writes the checkpoints, so
signing protects the log against someone with access to the store, not
against someone who has compromised that process. The destination MUST be
append-only or remote: a process that can rewrite the file can also rewrite
the checkpoints. A verifier checks only the checkpoints it is given, so
deleting or withholding the newest ones silently shortens the protected
window back to the newest one that remains. Keeping the set complete is the
operator's job.

Signing is optional and settles issue #60:

- **Algorithm**: Ed25519 (RFC 8032), from the Go standard library. The
  signature covers the bytes `"bearing.audit.checkpoint.v1"`, a NUL, and the
  canonical encoding of the checkpoint without its `signature` field, so the
  sequence number, hash, time and key ID are all signed.
- **Keys**: the key ID is a label the operator picks (1 to 64 letters,
  digits and `. _ : -`). Private keys are PKCS #8 PEM and public keys PKIX
  PEM, as `openssl genpkey -algorithm ed25519` and `openssl pkey -pubout`
  write them. There is no certificate chain.
- **Who holds what**: the process that writes checkpoints holds the private
  key as a secret, named by reference like any other (C-SECRET-1), and the
  people who can write to the store don't. A verifier needs only public keys,
  so an auditor can check the log without being able to sign.
- **Trust**: a verifier trusts exactly the public keys it is given, by key
  ID, out of band. It never reads a key from the store or the checkpoint
  file. A checkpoint that names a key it does not hold fails, as does one
  whose signature does not match. An unsigned checkpoint fails too, unless
  the caller sets `AllowUnsigned` for a log whose operator signs nothing, so
  whoever controls the checkpoint file can't swap signed lines for unsigned
  forgeries.
- **Rotation and compromise**: a new key gets a new ID and a checkpoint is
  written under it at once, and the verifier keeps the public keys of old
  checkpoints. If a signing key leaks, checkpoints under it can no longer be
  told from forgeries: the operator verifies the log by other means (for
  example against an independent backup), writes a checkpoint under a new
  key, and gives the verifier the new key and only the checkpoints from the
  new one on. The new checkpoint pins the chain as it stands.
- The signature says the checkpoint's writer held the key. The time in it is
  the writer's claim.

### Verify

`audit.Verify` reads the log from its oldest record, in pages, and reports
what it finds, naming the first bad record by `seq`:

- a record whose hash doesn't match its content (an edit);
- a gap (records missing: a deletion), or a record whose `prev_hash` isn't
  the one before it (an insertion or a replaced record);
- an oldest record that isn't record 1 (a cut-off start). No checkpoint
  excuses a missing start, since nothing marks one as written at a retention
  cut; that marker, a signed field of the checkpoint, comes with retention;
- a checkpoint whose hash differs from the record at its `seq` (a chain
  rewritten end to end);
- a checkpoint newer than the log's newest record (a cut-off tail);
- a checkpoint that is unsigned (unless allowed) or has a bad or untrusted
  signature, which is also not used to judge the chain.

It stops reading at the first broken record. `bearing audit verify` runs it
against a store, a checkpoint file and public keys, and exits non-zero when
anything fails. What it can't find is a rewrite of records newer than the
latest checkpoint it is given, so the interval bounds how much recent history
someone with store access can change undetected, and a truncation back to the
newest checkpoint still held if the newer ones were withheld.

### Storage

The in-memory store keeps the records in a slice. The PostgreSQL store keeps
them in `audit_record`, one row per record holding the record as marshaled
and the columns `Query` filters on, and the newest record's number, hash and
time in the `meta` row that every `Apply` already locks. That lock is what
makes the chain: the next record is built from the head read under it, in the
transaction that writes the change. `Restore` empties the table with the rest
of the graph and the replay writes it again. A store that applied events
before the log existed has no records for them (no released version did), and
a restore from its backup would write them.

### Query

`Query` takes an `AuditFilter`: records after a sequence number, up to a
limit of 1 to `MaxAuditQueryRecords` (1,000, required), narrowed by event ID,
actions, actor ID, target kind and ID (an ID requires its kind) and a
`recorded_at` range `[From, To)`. It returns the matching records oldest
first and at most `MaxAuditQueryBytes` (32 MiB) of them, always at least one
if any match (the size is the Protobuf wire size of the records). A caller
pages with the last `seq` it saw until a call returns nothing: a short page
does not mean the end. A filter outside the bounds fails with
`ErrInvalidAuditQuery` (`CheckAuditFilter`): a limit outside 1 to 1,000, an
action that is unset or unknown or more actions than there are, an event ID
over `MaxEventIDBytes` or not text (invalid UTF-8 or a NUL), an actor or target ID over `MaxAuditIDBytes`, an
unknown target kind, a target ID without its kind, or `From` not before `To`
when both are set. `Head` returns the newest record's `seq`, hash and
`recorded_at`, or the zero value for an empty log. Records name people, so the API that exposes `Query` is for the admin
role only ([ADR 12](../adr/0012-authentication-through-oidc.md)).

### Not implemented yet

- `bearing audit verify` and `bearing audit checkpoint` (issue #138).
- Retention. The contract has no delete. The cut that removes the oldest
  records comes with the server (issue #139), together with the signed marker
  on a checkpoint that says it was written at a cut; until then `Verify`
  accepts only a log that starts at record 1.
- Export to object storage or a SIEM (ADR 8).
- Writers that change no graph state (policy decisions, executor actions).

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

[`internal/memstore`](../../internal/memstore) is the reference `GraphStore`,
`VectorIndex` and `EventLog` and shows the pattern.
[`internal/pgstore`](../../internal/pgstore) passes the `VectorIndex`,
`GraphStore` and `EventLog` suites. A backend that keeps events past the
process also tests that a second store on the same data sees them, since the
suite cannot restart it.
