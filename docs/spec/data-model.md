# Data model

Version 0.3 (draft). The rule sections (from [Terms](#terms) to
[Wire mapping](#wire-mapping)) are normative: statements there in the
imperative or present tense ("is", "becomes", "writes") are requirements,
as if they said MUST. Worked examples, open questions and paragraphs
starting "Note:" are not.

This model replaces the v0.1 scaffold outright. Nothing here is kept
compatible with it.

## Changes in this revision

| Version | Change | Why |
| --- | --- | --- |
| 0.2 | A **subject** with a Bearing-minted, never-reused ID is what everything attaches to. Source keys are its **aliases**, `id` (immutable) or `name` (time-bounded). | Renames and transfers keep the subject; reused names get a new one. |
| 0.2 | Exact resolution, minting, merge and un-merge rules, audited. | Identity is where implementations would diverge. |
| 0.2 | **Facts** `(subject, predicate, object)`; attributes are facts; one predicate registry. | One model with provenance, time and confidence for everything. |
| 0.2 | **Bitemporal**: claims write states onto valid time; supports have record time. "As of" and "what changed" queries. | Time-bounded facts are core, not special cases. |
| 0.2 | Per-source supports, latest `observed_at` wins per valid time, snapshot scopes, sync completeness. | A stale sync can't undo a newer event; endings are explicit. |
| 0.2 | Confidence in parts per million: max within a source system, noisy-OR across. Statuses and conflicts. | Corroboration counts once per independent system; disputed owners are never asserted. |
| 0.3 | The identity store, old history and the effects of manual events are primary state, backed up with the graph ([ADR 11](../adr/0011-identity-store-is-primary-state.md)). | Subject IDs must survive events ageing out of the log. |
| 0.3 | Adapters **declare** which registered kinds and predicates they emit, their key types, matching fields, links and default **authority** in `Describe`; operators override authority per source. Replaces the spec's key type registry. Kinds and relations still come only from this spec. | A new source needs no spec change for its keys or matching. |
| 0.3 | One generic **matcher** scores same-kind pairs; the score is the confidence of `same_as`. **Merge policy** per kind: people need authoritative evidence or confirmation, teams merge on score. Replaces the fixed identity rule table. | Matching rules belong in configuration, not the spec. |
| 0.3 | CODEOWNERS is reported literally as `approves_changes`; the core **derives** `owned_by` from the file's shape. | CODEOWNERS names reviewers; only some files say who owns the repository. |
| 0.3 | Authority resolves conflicts by default. | Most disagreements have an obvious source of record. |
| 0.3 | Tiered **compaction** with a precision marker on answers. | History can't grow without bound. |
| 0.3 | Protobuf is the only source of truth; core-closed lists are proto enums. | No hand-kept copies. |

## Terms

| Term | Meaning |
| --- | --- |
| Subject | A thing Bearing knows about, identified by a `subject_id`. |
| Kind | A subject's type (`Person`, `Repository`, …), fixed at mint. |
| Issuer type | A kind of identifier issuer, such as `github`, `authentik` or `saml`. Adapters declare its key types. |
| Namespace | One issuer instance, for example `github` (github.com) or `ghes-acme`. Each has an issuer type. |
| Alias | `(namespace, key_type, external_id)`, written as a key `<namespace>:<key_type>/<external_id>`. |
| Source | A configured adapter instance ([ADR 10](../adr/0010-configuration-as-resources.md)), for example `github-acme`. The unit of provenance. |
| Source system | The namespace a source reads. Sources of one system are not independent. |
| Declaration | What an adapter's `Describe` says it emits: kinds, key types, fields and links ([Declarations](#declarations)). |
| Authority | Whether a source is the source of record for a field of a kind. Declared by the adapter, overridable per source. |
| Observation | One source's report about one entity at one `observed_at`. |
| Claim | One statement in an observation (or a manual event) about one fact. |
| Support | One source's claims about one fact, evaluated over valid time. |
| Valid time | When something is true in the world, `[valid_from, valid_to)`. |
| Record time | When Bearing held it, `[recorded_at, retracted_at)`. |

Intervals are half-open. In stored and reported intervals, `null` at either
end means unbounded (claims default differently, see [Claims](#claims)). Times are
RFC 3339 UTC with `Z`, stored and compared at microsecond precision
(finer digits are truncated).

## State, determinism and apply

Bearing's state has two parts:

- The **identity store**: subjects, alias bindings, and merge and un-merge
  records. It is **primary state**, backed up with the graph and never
  rebuilt from events, because identity decisions depend on apply order and
  the event log keeps only a window ([ADR 7](../adr/0007-durable-event-log.md)).
  It is never compacted.
- The **claim store**: claims, watermarks, support versions and audit
  links. It is rebuilt by replay only for valid and record times inside the
  event log's retention window. History older than that, compaction
  summaries and the effects of manual events are primary state, backed up
  with the graph. Manual events MUST be retained as long as their effects
  are live ([ADR 11](../adr/0011-identity-store-is-primary-state.md)).

Guarantees:

1. Given the same identity decisions (the same mints, bindings, merges and
   rejections, for example by replaying against the final identity store),
   the same configuration and the same set of events, conforming
   implementations produce the same valid-time state: supports, fact
   statuses, confidence and conflicts at every valid time not yet
   [compacted](#retention-and-compaction), **regardless of the order** the
   events are applied in. (The event log orders events only within a
   partition.)
2. Applying the same events in the same order from the same identity store,
   they make the same identity decisions, up to a one-to-one renaming of
   newly minted subject IDs.
3. `recorded_at`/`retracted_at` values may differ between implementations.

**Apply and isolation.**

- Each event is applied in one transaction: its identity changes, claims,
  watermarks, audit records and the processed-event mark (ADR 7).
- `recorded_at` comes from a store sequence: a single clock record, read
  and advanced in each apply transaction to `max(now, previous + 1 µs)`.
  Every apply therefore has a unique, strictly increasing `recorded_at`.
- Because every apply writes the clock record, applies are serialized.
  This is the only isolation model in this version. It is far above MVP
  volume; an M3 load benchmark against SurrealDB checks the ceiling
  ([issue #27](https://github.com/dhm116/bearing/issues/27)). It can later
  be relaxed to per-partition hybrid logical clocks, with an as-recorded
  watermark below which every partition's applies are complete, so
  record-time reads stay consistent.

## Subjects

```json
{ "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "kind": "Repository",
  "status": "active", "merged_into": null, "minted_at": "2026-09-28T01:30:02Z",
  "minted_by": { "event_id": "github-acme/9f2c…", "rule": "observation" } }
```

| Field | Rule |
| --- | --- |
| `subject_id` | UUIDv7 ([RFC 9562](https://www.rfc-editor.org/rfc/rfc9562)), canonical lowercase text. Minted only by the core, from one **monotonic** generator (RFC 9562 §6.2) whose state lives with the clock record, in apply order, so a later mint always has a greater ID. Never changed, never reused. Opaque to consumers. |
| `kind` | A [subject kind](#subject-kinds), fixed at mint. |
| `status` | `active` or `merged`. Subjects are never deleted. |
| `merged_into` | For `merged` subjects, the subject it was merged into. Reads follow `merged_into` to an `active` subject. |
| `minted_by.rule` | `observation`, `reference` or `split` ([Minting](#minting)). |

Note: UUIDv7 is a standard with native database types, and its mint-time
order gives merges a deterministic survivor.

A subject with no `exists` claim ever recorded (only referenced) is a
**placeholder**.

## Identifiers and aliases

### Keys and namespaces

```
<namespace>:<key_type>/<external_id>
github:repo_node/R_kgDOH1a2b3
authentik:user/7f3c2a9e-1b4d-4c8e-9a0f-2d6e8b1c5a74
```

`namespace` and `key_type` are lowercase letters, digits and hyphens
(`key_type` also allows `_`). `external_id` is the issuer's value and MAY
contain `/` and `:`. A key is parsed by splitting at the first `:` and the
first `/` after it.

Namespaces come from source configuration:

| Source config field | Meaning |
| --- | --- |
| `namespace` | The namespace the source reads. Default: its adapter's issuer type (`github` for github.com). A GitHub Enterprise Server source sets e.g. `ghes-acme`. |
| `issues` | Other namespaces the source is the issuer for: `[{ "namespace": "authentik-saml", "issuer_type": "saml", "key_classes": {} }]`. |
| `links` | Namespaces the source reports `linked_ids` in, with their issuer types. |
| `authority` | Overrides of declared authority, one per field, link or derived source (below). |

An `authority` override names exactly one target and sets its `authority`:

```json
[ { "kind": "Repository", "predicate": "owned_by", "direction": "out", "authority": { "authoritative": false } },
  { "kind": "Person", "link": { "issuer_type": "github", "key_type": "user" }, "authority": { "authoritative": true } },
  { "derived": "codeowners", "authority": { "authoritative": true } } ]
```

`derived` overrides the authority of `core/derive/<rule>/<this source>`.

A key in a namespace the source neither reads, issues nor links is rejected
(`namespace_not_allowed`). Every configured use of a namespace MUST name the
same issuer type; configuration apply rejects a disagreement. Key types are
looked up by the namespace's issuer type in the
[declarations](#declarations) in force.

### Key types

Adapters declare key types per kind ([Declarations](#declarations)):

| Property | Meaning |
| --- | --- |
| `issuer_type` | Default: the adapter's own. |
| `key_type` | The name in keys. Its kind is the kind it is declared under; one key type has one kind. |
| `class` | `id`: assigned once, never changed or reassigned. `name`: can be renamed and later reused. |
| `per_subject` | `name` only. `one`: binding a new name of this type to a subject releases its previous one. |
| `redirects` | `name` only. A released name still resolves to its last subject (as GitHub does for repositories). |
| `case` | `insensitive` external IDs are compared and stored after Unicode simple case folding. Default `sensitive`. |

- Any adapter may declare key types of any issuer type (its own, those it
  issues and those it links). Configuration apply rejects two declarations
  of one `(issuer_type, key_type)` that differ, and a configured link or
  issued namespace whose key types nobody declares.
- A source's `issues[].key_classes` MAY override a key type's class for that
  namespace: a SAML namespace whose NameID format isn't `persistent` sets
  `{ "name_id": "name" }`.
- A key whose key type isn't declared for its namespace's issuer type is
  rejected (`not_declared`).
- Declarations are configuration: versioned with record time like the rest
  (ADR 10), and taken from the adapter version a source runs. Once any
  alias of a key type is bound, its kind, class and case are fixed;
  configuration apply rejects a declaration, or an `issues[].key_classes`
  override, that changes them.
- GitHub `id` keys are next-format global node IDs (the adapter sends
  `X-Github-Next-Global-ID: 1`). Legacy node IDs MUST NOT be emitted. Node
  IDs survive renames, transfers and login changes. Directory `id` keys are
  the directory's immutable primary keys.

### Declarations

An adapter's `Describe` result ([adapter protocol](adapter-protocol.md#bearingdescribe))
declares, per kind it emits:

| Field | Meaning |
| --- | --- |
| `keys` | Its [key types](#key-types): which are permanent ids and which renamable names. |
| `fields` | Every predicate it claims with this kind as the observed entity: `predicate`, optional `direction` (`in` for relations given with `from`; default `out`), `match` (`exact`, `email`, `name` or `members`, if the field is [identity evidence](#matching)) and `authority` (`{ "authoritative": true }`; default not authoritative). An unregistered attribute also gives `type` (`string`, `float`, `bool`, `time`, `json`) and `cardinality`; it is stored as `<namespace>.<attribute>`. |
| `links` | Key types of other systems that this system records for the entity (`linked_ids`), with `authority`. |

A **field** is `(source, kind, predicate, direction)`. A support's
**authority** is evaluated at read time `r` from the declarations and
`authority` overrides in force at `r`; it is not stored on the claim. A
source's own keys are always authoritative in its namespace. Kinds and
relation predicates come only from this spec's registry; adapters add only
namespaced attributes. A claim of an undeclared kind, predicate, key type
or link is rejected (`not_declared`); `exists` is implicit.

Reference declarations for the MVP sources (attributes without a role
abbreviated):

```json
{ "name": "github", "issuer_type": "github", "kinds": [
  { "kind": "Repository",
    "keys": [ { "key_type": "repo_node", "class": "id" },
              { "key_type": "repo", "class": "name", "per_subject": "one", "redirects": true, "case": "insensitive" } ],
    "fields": [ { "predicate": "approves_changes", "authority": { "authoritative": true } },
                { "predicate": "default_branch", "authority": { "authoritative": true } }, { "predicate": "name" },
                { "predicate": "codeowners_rules", "type": "float", "cardinality": "one" }, "…" ] },
  { "kind": "Team",
    "keys": [ { "key_type": "team_node", "class": "id" },
              { "key_type": "team", "class": "name", "per_subject": "one", "case": "insensitive" } ],
    "fields": [ { "predicate": "name", "match": "name" }, { "predicate": "slug" },
                { "predicate": "member_of", "direction": "in", "match": "members", "authority": { "authoritative": true } } ] },
  { "kind": "Person",
    "keys": [ { "key_type": "user_node", "class": "id" },
              { "key_type": "user", "class": "name", "per_subject": "one", "case": "insensitive" } ],
    "fields": [ { "predicate": "name", "match": "name" }, { "predicate": "verified_email", "match": "email" },
                { "predicate": "commit_email", "match": "email" }, { "predicate": "login" } ],
    "links": [ { "issuer_type": "saml", "key_type": "name_id", "authority": { "authoritative": true } } ] } ] }
```

```json
{ "name": "authentik", "issuer_type": "authentik", "kinds": [
  { "kind": "Person",
    "keys": [ { "key_type": "user", "class": "id" },
              { "key_type": "username", "class": "name", "per_subject": "one", "case": "insensitive" },
              { "issuer_type": "saml", "key_type": "name_id", "class": "id" } ],
    "fields": [ { "predicate": "name", "match": "name", "authority": { "authoritative": true } },
                { "predicate": "email", "match": "email", "authority": { "authoritative": true } } ],
    "links": [ { "issuer_type": "github", "key_type": "user_node", "authority": { "authoritative": true } },
               { "issuer_type": "github", "key_type": "user" } ] },
  { "kind": "Team",
    "keys": [ { "key_type": "group", "class": "id" },
              { "key_type": "group_name", "class": "name", "per_subject": "one" } ],
    "fields": [ { "predicate": "name", "match": "name" },
                { "predicate": "member_of", "direction": "in", "match": "members", "authority": { "authoritative": true } } ] } ] }
```

The `github` `user_node` and `user` key types that Authentik links are
declared by the GitHub adapter; both declarations of `saml` `name_id` would
have to agree.

### Bindings

- An **`id` alias** is bound to one subject for all valid time.
  Resolution ignores time for `id` aliases. Only an un-merge moves one.
- A **`name` alias** maps valid time to a subject (or to "released"). Each
  observed binding is a write like a fact claim ([Claims](#claims)): an
  observation binding name `N` to `S` at `t` writes `S` on `[t, ∞)`; a
  release (`entity.deleted`) writes "released" on `[t, ∞)`. At each valid
  time the covering write with the greatest
  [ordering key](#ordering-and-idempotency) wins. Before the name's earliest
  observed binding, it maps to that binding's subject (back-extension).
- **`per_subject: one`** is a read-time rule: at each valid time, a subject
  holds, per key type, only the name whose covering write to it has the
  greatest key; other names that would map to it there are released.
  Back-extension never releases a name, and a back-extended mapping is
  dropped where the subject holds another name by a covering write.
- A binding written by a reference (when minting a placeholder) is
  **tentative**. It has no start: it covers every valid time at which the
  name is unbound or released without redirect; an observed binding to a
  subject overrides it.

### Resolution

An observed entity is resolved from its `key`, `aliases` and `observed_at`
(`t`):

1. **Id match.** Look up its `id` aliases. Any bound subject whose kind
   differs from `entity.kind`: reject the observation (`kind_mismatch`).
   One subject: use it. Several: they are one thing; merge them (rule
   `co_reported_ids`), unless a `distinct_from` between them is live, in
   which case reject the observation (`identity_conflict`) and open a
   conflict on `(subject, same_as)`.
2. **Name match.** Else look up its `name` aliases at `t`. For a subject
   `S` found:
   - If `S` holds an `id` alias of a key type the observation also carries,
     with a different value, the name was reused: skip `S`.
   - Otherwise use `S`; it adopts the observation's `id` aliases. If names
     give two subjects, use the lower `subject_id` and open a conflict on
     `(subject, same_as)`; do not merge.
3. **Mint** a subject of `entity.kind` (rule `observation`).

Then write bindings for every alias at `t`. If the observed binding just
written maps the name to the chosen subject at a valid time at which a
reference resolved it to placeholder `P` through a tentative binding, `P`
merges into the chosen subject (rule `placeholder`). The outcome is the
same whichever of the reference and the observation is applied first.

A **reference** (a relation's `to` or `from`, or a `linked_id`) resolves to
the subject its alias maps to at `t`, tentative bindings included. A
released name resolves to the subject it last mapped to if the key type
`redirects`. Otherwise, and for an unbound key, a placeholder of the key
type's kind is minted (rule `reference`) with a tentative binding, so later
references to the same name reuse it.

Each claim records `via`: the set of aliases the observation's entity
carried, and the reference key for the other end.

### Minting

| Rule | When |
| --- | --- |
| `observation` | Resolution rule 3. |
| `reference` | A reference to an unbound alias, or to a released name that doesn't redirect. |
| `split` | An un-merge whose aliases never had a subject of their own. |

Nothing else mints. Every mint is audited.

### Renames, moves and reuse

| Event | Effect |
| --- | --- |
| GitHub repository renamed or transferred | Same `repo_node`, so same subject. The old `repo` name is released and redirects. |
| GitHub team slug renamed | Same subject. The old slug is released; references to it mint a placeholder. |
| GitHub login changed | Same subject. |
| Name reused by a new repository, user or group | Different `id`, so a new subject. The old subject keeps the name before `t`. |

### Merge

| Rule | Trigger | Confidence |
| --- | --- | --- |
| `co_reported_ids` | Resolution rule 1. | 1.0 |
| `placeholder` | An observed binding covers a placeholder's tentative one. | 1.0 |
| `authoritative` | Live [authoritative evidence](#identity-across-systems) between them, under any merge policy but `manual`. | 1.0 |
| `score` | `same_as` confidence reaches the kind's threshold under the `score` [merge policy](#merge-policy). | computed |
| `manual` | A `MergeRequested` event. Rejected (`identity_conflict`) while a `distinct_from` between them is live, unless the request also clears it. | 1.0 |

Only subjects of the same kind merge. **Guard:** no rule but
`co_reported_ids` and `manual` joins two subjects that hold different `id`
aliases of one key type in one namespace: their `same_as` is `conflicted`,
a conflict opens, and the pair is listed by
[`data_quality`](#data-quality) as `id_conflict`. If a subject has accepted
`same_as` to two or more subjects that the guard excludes from each other,
none of those pairs merges; all are `conflicted` and listed the same way.
Across applies, the first merge stands and the later pair is flagged.

**When merges happen.** Each apply evaluates merge triggers after writing
its claims and derived claims, for every pair of active subjects whose
`same_as` supports, member sets, `distinct_from` or guard the apply
changed, and, after a configuration change to merge policies, thresholds
or match weights (applied as an event, ADR 10), for every pair with a live
`same_as` support. A pair merges if its kind's
policy accepts `same_as` at any valid time `v` ≤ the applied event's
`observed_at` (the ingest time for manual, boundary and derived events).
Qualifying pairs are taken in order (confidence descending, then lower
`subject_id`, then higher), and the rest are re-evaluated after each merge,
with canonicalization and the guard applied, until none qualifies. A pair's
ordering confidence is its highest `same_as` confidence over the valid
times at which the policy accepts it.

At record time `r`:

1. The **survivor** is the lower `subject_id` (the earlier mint). The other
   gets `status: merged`, `merged_into: <survivor>`.
2. Its alias bindings count for the survivor. Valid times are unchanged.
3. Stored claims keep their subject IDs. From `r`, reads canonicalize
   subject and object through `merged_into`, so the merged subject's
   supports count for the survivor's facts. Fact IDs are computed from the
   canonical IDs. Snapshot scopes and watermarks match facts after
   canonicalization. Same-source supports combine by the ordering rule, and
   a `one` predicate keeps at most one object per source
   ([Supports](#supports)).
4. `same_as` and `distinct_from` facts between the two move to the merge
   record and keep being evaluated. A later `same_as`/`distinct_from` claim
   whose ends canonicalize to one subject counts as evidence for the merge
   records joining the subjects its `via` aliases were bound to. An
   `authoritative` or `score` merge's evidence is re-evaluated as if the two
   were separate: each side has only the claims whose `via` aliases (and,
   for `members`, the `via` of each `member_of` object) are in its alias set
   in the merge record, so the merged subject never confirms itself. The
   check uses the same rule as merge triggers. If the evidence now
   contradicts the merge (a link names a different id), the core opens a
   conflict on `(subject, same_as)` for review. The per-side score of a
   `score` merge is reported on the merge record for review. Evidence that
   ends because a subject was deleted or lost its live `exists` is not a
   reason for review. The core never un-merges by itself.
5. Reads as recorded before `r` still show two subjects.
6. An audit record holds the rule, confidence, evidence, both alias sets and
   the event ID.

### Un-merge

Only an `UnmergeRequested` event un-merges. It names an active subject `A`
and a non-empty proper subset `D` of `A`'s aliases. A `placeholder` merge
can't be un-merged: it is correct by construction. At record time `r`:

1. If `D` is the alias set some `B` had when it merged into `A`, `B` becomes
   `active` again; subjects earlier merged into `B` stay merged into `B`.
   Otherwise mint a subject (rule `split`).
2. Binding writes of aliases in `D` that map them to `A` are re-pointed to
   it, for all valid time.
3. A claim whose `via` set is within `D` gets a re-pointing record (stored
   claims are not rewritten) and counts for it. Claims whose `via` spans
   both sides, or that have none (manual, core), stay with `A` and are
   listed in the audit record for review.
4. A `distinct_from` between the two is set (source `manual`), which blocks
   them from merging again by score or evidence.

A `DistinctFromSet` for two subjects already merged is rejected
(`already_merged`); un-merge instead.

## Subject kinds

| Kind | Meaning | Registered attributes | MVP sources |
| --- | --- | --- | --- |
| `Person` | A human | `name`, `email`, `login`, `verified_email`, `commit_email` | GitHub, directory |
| `Team` | A group of people | `name`, `slug`, `description` | GitHub teams, directory groups |
| `Repository` | A source repository | `name`, `full_name`, `url`, `default_branch`, `language`, `topics`, `archived`, `description` | GitHub |
| `Component` | A service, library, job or website | `type`, `tier`, `lifecycle` | |
| `Package` | A published package | `ecosystem`, `name`, `versions` | |
| `Environment` | Where components run | `name`, `account`, `region` | |
| `CloudResource` | An infrastructure resource | `arn`, `type`, `tags` | |
| `Change` | A pull request, commit, deploy or config change | `kind`, `at`, `url` | |
| `Incident` | An operational incident | `severity`, `status`, `started_at` | |
| `Schedule` | An on-call rotation | `name` | |
| `Document` | A runbook, ADR, README or similar | `title`, `url`, `doc_type` | |

A change's author is its `changed_by` fact. An on-call source reports
`on_call_for` facts with `valid_from`/`valid_to` per shift, and "who is on
call now" is an [as-of query](#as-of). Adapters emit only kinds they
[declare](#declarations).

## Predicates

| Property | Meaning |
| --- | --- |
| `name` | `[a-z][a-z0-9_]*`, or `<namespace>.<attr>` for unregistered attributes. |
| `domain` / `range` | Subject kinds; range is kinds (a **relation**) or a value type (an **attribute**). |
| `cardinality` | Per source. `one`: at each valid time a source supports at most one object. `many`. |
| `conflict` | Across source systems: `none`, `one` or `set` ([Conflicts](#conflicts)). |
| `threshold_ppm` | Assertion threshold. Default 900000. |

A claim outside domain or range is rejected (`domain_mismatch`); a value of
the wrong type is rejected (`type_mismatch`). The rest of the observation
applies.

| Relation | Domain → range | Cardinality | Conflict |
| --- | --- | --- | --- |
| `member_of` | Person, Team → Team | many | none |
| `owned_by` | any → Team, Person | many | set |
| `approves_changes` | Repository → Team, Person | many | none |
| `depends_on` | Component → Component, CloudResource | many | none |
| `publishes` | Repository, Component → Package | many | none |
| `consumes` | Component → Package | many | none |
| `deployed_to` | Component, CloudResource → Environment | many | none |
| `runs_on` | Component → CloudResource | many | none |
| `on_call_for` | Schedule, Person → Component, Team | many | none |
| `affected_by` | Component → Incident | many | none |
| `documents` | Document → any | many | none |
| `changed_by` | Change → Person | many | none |
| `defined_in` | Component, Document → Repository | one | one |

| Attribute | Type | Cardinality | Conflict |
| --- | --- | --- | --- |
| `exists` (any kind) | bool | one | none |
| `default_branch`, `archived` | string, bool | one | one |
| `email`, `verified_email`, `commit_email`, `topics`, `versions` | string | many | none |
| `tags` | json | one | none |
| `at`, `started_at` | time | one | none |
| every other registered attribute in [kinds](#subject-kinds) | string | one | none |

- `exists` is claimed implicitly by every observation of an entity.
- `approves_changes`: the source requires the object's approval for changes
  to some paths ([CODEOWNERS](#codeowners)). It is not ownership.
- `verified_email`: an address the source verified on a domain the
  organization verified. `commit_email`: an author address of commits the
  source links to the person.
- An unregistered attribute is declared with its type and cardinality and
  becomes `<namespace>.<attr>` (namespace of the source system), conflict
  `none`.

**Core predicates** (adapters MUST NOT claim them): `same_as` and
`distinct_from`, between two subjects of one kind. Both are symmetric and
stored with the lower `subject_id` as subject.

| Value type | Canonical form |
| --- | --- |
| `string` | UTF-8 as given, compared byte for byte. |
| `float` | IEEE 754 double; `-0` becomes `0`; NaN and infinities rejected. |
| `bool` | `true` / `false`. |
| `time` | UTC, exactly 6 fraction digits: `2026-09-28T01:30:00.000000Z`. |
| `json` | [RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) (JCS). |

There is no integer type until a predicate needs one.

## Observations

Example (CloudEvents envelope fields `specversion`, `type`,
`datacontenttype` omitted here and below):

```json
{
  "id": "github:repo_node/R_kgDOH1a2b3@2026-09-28T01:30:00.000000Z",
  "source": "adapter/github",
  "time": "2026-09-28T01:30:00Z",
  "data": {
    "entity": {
      "kind": "Repository",
      "key": "github:repo_node/R_kgDOH1a2b3",
      "aliases": ["github:repo/acme/payments-api"],
      "attributes": { "name": "payments-api", "default_branch": "main", "language": "Go",
                      "topics": [], "description": null, "codeowners_rules": 1 }
    },
    "relations": [
      { "type": "approves_changes", "to": "github:team/acme/payments",
        "attributes": { "pattern": "*", "file": ".github/CODEOWNERS", "line": 1 } }
    ],
    "snapshots": [ { "direction": "out", "predicates": ["approves_changes"] } ],
    "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" }
  }
}
```

| Field | Rule |
| --- | --- |
| `time` | `observed_at` of every claim; see below. |
| `entity.key` | Primary key; SHOULD be `id` class when the source has one. |
| `entity.aliases` | More keys for the entity, only in the source's `namespace` or its `issues` namespaces. |
| `entity.linked_ids` | Keys in the source's `links` namespaces that the source records for the entity, of key types in the kind's declared `links`. Identity evidence, never an alias. When present, the list is complete for this source. |
| `entity.attributes` | Attribute claims. A value, an array (the complete set) or `null` (none). |
| `entity.deleted` | The entity no longer exists in the source. |
| `relations[]` | `type`; exactly one of `to` (entity is subject) or `from` (entity is object); `attributes` (qualifiers, not part of the fact); `absent`; optional `valid_from`, `valid_to`, `confidence_ppm`. |
| `attribute_claims[]` | Attribute claims needing times or confidence: `{ "predicate", "value", "valid_from", "valid_to", "confidence_ppm", "absent" }`. |
| `snapshots[]` | [Snapshot scopes](#snapshot-scopes). |
| `evidence` | Where a person can check the claims; stored on each support. |
| `id` | Stable for the same entity and time; part of the ordering key. |

- Provenance comes from the configured source whose event carried the
  observation, never from the CloudEvents `source` field. The core sets it
  in the extension attribute `bearingsource`, and event IDs include it
  (`<source>/<delivery or content id>`), so two sources' delivery IDs can't
  collide.
- Source names `manual` and anything starting `core/` are reserved.
- An adapter that read an attribute and found it empty MUST send `null`
  (or `[]` for a `many` predicate). Omitting an attribute means "not read".

### observed_at

- **Sync:** the earliest send time among the requests whose responses the
  observation reports (taken before the read), so a concurrent change is
  never shadowed.
- **Webhook:** the event time the source's **server** assigned, if the
  payload has one. Never a time an author can set (commit timestamps).
  Otherwise the adapter treats the delivery as a trigger and re-reads the
  entity (`observed_at` = that request's send time), or uses the delivery's
  original send time, never a redelivery time.
- Adapter and core clocks MUST be NTP-synchronized. The core rejects an
  `observed_at` more than 5 minutes after the event's ingest time
  (`observed_at_in_future`).

### Normalization

Each observation becomes claims about the resolved subject `E` at
`observed_at` `t`. Some inputs also act as **implicit snapshot scopes**
(below), so they end things regardless of apply order.

| Input | Claims | Implicit scope |
| --- | --- | --- |
| any entity | `(E, exists, true)` | |
| `entity.deleted` | none | `(source, E, out and in, *)`; releases `E`'s names in the source's namespaces at `t`; ends its `linked_ids` |
| attribute `k: v` | `(E, k, v)` | `(source, E, out, [k])` if `k` is `one` |
| attribute `k: [v…]` | one per element | `(source, E, out, [k])` |
| attribute `k: null` | none | `(source, E, out, [k])` |
| relation `to: T` / `from: F` | `(E, type, T)` / `(F, type, E)`, ended if `absent` | `(source, E, out, [type])` if `type` is `one` and `to` |
| `attribute_claims[]` | `(E, predicate, value)`, ended if `absent` | as for attributes |
| `linked_ids` | evidence for `(E, same_as, target)` ([Identity across systems](#identity-across-systems)) | the source's linked-id evidence for `E` |

Otherwise the absence of a fact is **not** an ending: adapters see
different slices at different times.

Within one observation:

- A fact claimed more than once (two CODEOWNERS lines naming one team) is
  one claim. Its qualifiers are the de-duplicated array of the claims'
  `attributes` objects, sorted by JCS bytes. Duplicates that disagree on
  `valid_from`, `valid_to`, `confidence_ppm` or `absent` reject the
  observation (`duplicate_claim`).
- Asserting and ending the same fact, or giving one predicate in both
  `attributes` and `attribute_claims`, rejects the observation
  (`duplicate_claim`). An array for a `one` predicate rejects it
  (`cardinality_mismatch`).

### Snapshot scopes

`snapshots` declares that the observation lists **every** fact this source
claims in a scope `(source, E, direction, predicates)`:

| Field | Meaning |
| --- | --- |
| `direction` | `out`: `E` is the subject. `in`: `E` is the object. |
| `predicates` | Names, or `["*"]` for all in that direction (attributes only `out`). `["*"]` ends every fact the source didn't repeat, so use it only when the observation carries everything the source knows about `E`. |

The observation's ordering key becomes the scope's **watermark**: an
ending on `[t, ∞)`, with that key, for every fact in the scope that the
observation does not claim, including facts whose claims are applied later.
An older claim applied afterwards can still say what was true before `t`,
but not after.

A scope MUST be complete within one observation; it cannot span pages. A
source MUST NOT declare one unless it read the complete set (not after a
permission error or a file it couldn't parse).

### Sync completeness

The last page of a sync (`done: true`) MAY declare
`complete_sync: { "kinds": ["Repository"] }`: the sync visited every entity
of those kinds the source can see. An adapter MUST declare it only when its
listing is stable under concurrent change (keyset or cursor pagination, or
a snapshot read); offset paging can skip items.

- The **sync** is identified by its `SyncRequested` event ID; `t0` is that
  event's time, earlier than every request the sync sends.
- A sync is **complete** when every page succeeded. If the core restarts
  it with an empty cursor ("reset"), that is a new sync and the earlier one
  is never complete.
- A subject of a declared kind that had a live `exists` support from this
  source at the first missing sync's `t0`, and is missing from **two
  consecutive** complete syncs (consecutive among complete syncs; failed
  syncs are skipped), is treated as `entity.deleted` at that `t0`, with
  watermark key `(t0, <sync ID>, <sync ID>, "")`.
- The core MAY append one derived deletion event per subject rather than
  apply them all in one transaction.

## Facts and supports

### Facts

```json
{ "fact_id": "041d6c02c06fa8bf4c8d093d9a3dce979580c4d9dfb7ead9b89542451890be89",
  "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "predicate": "owned_by",
  "object": { "subject_id": "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f" } }
```

- `object` is `{ "subject_id" }` or `{ "type", "value" }` (value in
  canonical form).
- `fact_id` is the lowercase hex SHA-256 of the JCS form of
  `{ "subject_id", "predicate", "object" }`, with canonical (survivor) IDs
  and value types written as in this document (`"string"`). Test vectors:

  | JCS input | `fact_id` |
  | --- | --- |
  | `{"object":{"subject_id":"0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f"},"predicate":"owned_by","subject_id":"0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e"}` | `041d6c02c06fa8bf4c8d093d9a3dce979580c4d9dfb7ead9b89542451890be89` |
  | `{"object":{"type":"string","value":"main"},"predicate":"default_branch","subject_id":"0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e"}` | `563ed892892a24d6ef4e160702db05949694a854bc3e1ae27e2b2af5dde1bba8` |

- A fact has no state of its own: status, confidence and valid time are
  computed from its supports.

### Claims

A claim writes a state onto valid time, with its ordering key:

| Claim | Writes |
| --- | --- |
| assert with `valid_from` `a` (absent or `null`: `observed_at`), `valid_to` `b` (absent or `null`: open), `confidence_ppm` `c` (absent: 1000000) | `asserted(c)` on `[a, b)`, and `ended` on `[b, ∞)` if `b` is set |
| assert with only `valid_to` `b` ≤ `observed_at` | `ended` on `[b, ∞)` (it says the fact ended at `b`) |
| end (`absent`) at `t` = `valid_to` if given, else `observed_at` | `ended` on `[t, ∞)` |
| watermark of a scope at `t` | `ended` on `[t, ∞)` for each fact in scope not claimed |
| withdraw (`ClaimWithdrawn` event) | `ended` on `(-∞, ∞)` |

An explicit `valid_to` ≤ `valid_from` is rejected (`invalid_interval`). An
assert with a default `valid_from` writes nothing before `observed_at`, so
re-sending it never moves its start. `confidence_ppm` is an integer in
`[1, 1000000]`; to say a fact is false, end it.

Adapters MAY backfill `valid_from` with a time the source records for when
the fact began (a membership start, a repository's creation), so history
doesn't start at install. They MUST NOT estimate one.

### Supports

A source's support for a fact at valid time `v`, as recorded at `r`, is the
state written at `v` by that source's greatest-key write about the fact
that covers `v` and was recorded at or before `r` (watermarks included). If
it is `asserted(c)`, the support is **live** with confidence `c`. For a
`one` predicate, at each `v` only the source's greatest-key write (asserted
or ended) among all objects of `(subject, predicate)` counts, after
canonicalizing through merges; its object is supported only if that write
is asserted.

Implementations MAY evaluate writes on read or materialize **support
versions**:

```json
{ "fact_id": "9b1e…", "source": "github-acme", "adapter": "github@0.2.0",
  "event_id": "github-acme/9f2c…", "observation_id": "github:repo_node/R_kgDOH1a2b3@2026-09-28T01:30:00.000000Z",
  "observed_at": "2026-09-28T01:30:00Z", "last_confirmed_at": "2026-10-01T06:00:00Z",
  "confidence_ppm": 1000000, "valid_from": "2026-09-28T01:30:00Z", "valid_to": null,
  "recorded_at": "2026-09-28T01:30:02Z", "retracted_at": null, "reason": "assert",
  "via": { "subject": ["github:repo_node/R_kgDOH1a2b3", "github:repo/acme/payments-api"],
           "object": "github:team/acme/payments" },
  "qualifiers": [ { "file": ".github/CODEOWNERS", "line": 1, "pattern": "*" } ],
  "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" } }
```

- One row per maximal valid-time interval with one state, confidence and
  qualifier set. `adapter` is the adapter name and version that produced
  the observation; derived supports have none.
- Rows are immutable except that `retracted_at` is set once, to the
  `recorded_at` of the apply that replaces them.
- `reason`: `assert`, `end`, `snapshot`, `deleted`, `withdrawn`, `merge`,
  `unmerge`, `derived`.
- A materialized store MUST keep the ordering key per valid-time segment. A
  **confirming** claim (same state, confidence and qualifier set, later key) replaces the keys on
  `[observed_at, ∞)` only, and creates no new support version.
  `last_confirmed_at` is informational.

Both forms give the same answers to every query here.

### Derived claims

The core derives some claims by configured **derivation rules**. A rule
reads one input source's live supports; matching
([Identity across systems](#identity-across-systems)) reads asserted facts.
A derived support's source is `core/derive/<rule>/<input source>`. It
counts in its input source's system for [Confidence](#confidence) and is
not authoritative unless an `authority` override says so. Its state at each
valid time is the rule applied to its inputs' state there, so it follows
them on valid time. It is re-evaluated, with reason `derived`, in every
apply that changes its inputs, including a `ValidTimeBoundaryReached`
event, which also re-evaluates matching and merge triggers for the
affected subjects.

#### CODEOWNERS

The GitHub adapter reports only the effective CODEOWNERS file (the first
found of `.github/`, the root and `docs/`, as GitHub looks them up): for
each rule line and each owner, `(Repository, approves_changes, owner)` at
1000000 with qualifiers `{ "file", "pattern", "line" }`, a snapshot scope
over `approves_changes`, and the attribute `github.codeowners_rules`, the
number of rule lines including those with no owner. Lines naming one owner
collapse into one fact whose qualifiers list every line.

The default configuration's rule `codeowners` reads the file's shape from
one source's live `approves_changes` supports and `codeowners_rules` for a
repository, and claims `owned_by` for each owner on a `*` line:

| File shape | `owned_by` for each `*` owner | Default ppm (configurable per shape) |
| --- | --- | --- |
| One rule line, pattern `*`, naming one team | yes | 950000 |
| Any other file with a `*` line (several owners, a person, or path rules too) | yes | 700000 (below threshold: `candidate`) |
| Only path rules | none | |

Note: a team appearing in many repositories' CODEOWNERS gives no ownership
signal.

### Retraction

Ending or withdrawing affects one source's support, never the fact. A fact
with no live supports at `v` has status `none` there; its history stays.
An ending sets `valid_to` (it stopped being true then). A withdrawal removes
the claim at all valid times from that record time on; reads as recorded
earlier still show it.

## Ordering and idempotency

Every write has an **ordering key**, compared left to right:

```
(observed_at, observation_id, event_id, content_hash)
```

`observation_id` and `event_id` (which includes the source) by UTF-8 bytes;
`content_hash` = SHA-256 of the JCS form of the ProtoJSON of the
observation's `data` (proto field names, enum value names, unset fields
omitted). All
four are fixed by the event, so the key doesn't depend on apply order. For
manual events, `observed_at` is the ingest time, `observation_id` the event
ID and `content_hash` that of the payload.

- **Latest wins per source, fact and valid time.** Different sources never
  override each other; they combine ([Confidence](#confidence)).
- **A stale full sync cannot undo a newer event.** A page requested at
  10:00 listing an owner loses, at valid times from 10:05, to a webhook
  ending it at 10:05, in either apply order.
- **Idempotency.** Re-applying a processed event is a no-op (ADR 7). A
  different event carrying an identical observation only confirms.

Note: the 10:00 page may come from a lagging replica of the source. Bearing can't
detect that; the next sync corrects it.

## Confidence

All confidence is integer **parts per million** (ppm). At `(v, r)`:

1. Group the fact's live supports by **source system**. `manual` is one
   group; `core/identity/link` and each `core/identity/match/<method>` is
   one group; a derived support joins its input source's system. Systems
   listed together in the configuration field `confidence_groups` (systems
   that copy each other) form one group.
2. Within a group, take the maximum: `g_k`.
3. Across `n` groups, exact integer noisy-OR:
   `P = ∏ₖ (1000000 − g_k)`, then
   `C = 1000000 − round_half_even(P / 1000000^(n−1))`, in arbitrary-precision
   integers (`P` exceeds 64 bits from `n = 4`).

| Live supports (system: ppm) | `C` | At 900000 |
| --- | --- | --- |
| github: 1000000 | 1000000 | asserted |
| github: 800000 and 600000 (two sources) | 800000 | candidate |
| github: 800000, catalog: 700000 | 1000000 − 60000 = 940000 | asserted |
| extractor: 600000, catalog: 500000 | 1000000 − 200000 = 800000 | candidate |
| a: 333333, b: 333333 | 1000000 − round(444444.888889) = 555555 | candidate |

## Status and conflicts

### Status

A fact's status at `(v, r)`, with a `status_reason`, is the first match:

| Step | Condition | Status | `status_reason` |
| --- | --- | --- | --- |
| 1 | No live supports | `none` | `no_support` |
| 2 | A live manual override covers `(subject, predicate)` | `asserted` if it lists the object, else `overridden` | `override` |
| 3 | `C` below the threshold | `candidate` | `below_threshold` |
| 4a | A `same_as` between subjects with a live `distinct_from`, where live authoritative evidence's covering write has a greater ordering key than the `distinct_from`'s covering write | `conflicted` | `conflict` |
| 4b | Any other `same_as` between subjects with a live `distinct_from` | `candidate` | `distinct_from` |
| 4c | A `same_as` the [merge guard](#merge) excludes | `conflicted` | `conflict` |
| 4d | A `same_as` its kind's [merge policy](#merge-policy) doesn't accept | `candidate` | `merge_policy` |
| 5 | Relation predicate with `conflict` ≠ `none`, and the object has no live `exists` support | `candidate` | `unobserved_object` |
| 6a | In a conflict, with precedence configured | `asserted` or `candidate` | `precedence` |
| 6b | In a conflict that authority decides | `asserted` or `candidate` | `authority` |
| 6c | In a conflict otherwise | `conflicted` | `conflict` |
| 7 | Otherwise | `asserted` | `none` |

Answers that act (ownership, policy) read only `asserted` facts
([ADR 2](../adr/0002-graph-is-source-of-truth.md)). A placeholder never
passes step 5, so a typo in CODEOWNERS never makes an owner; it is reported
as a [data-quality issue](#data-quality) instead.

A fact's reported `valid_from`/`valid_to` at `(v, r)` is the maximal
interval containing `v` over which its status, as recorded at `r`, is
unchanged.

Thresholds, precedence, authority overrides, groups, match weights, merge
policies, derivation rules and compaction tiers are those of the
configuration in force at record time `r`. Configuration changes are
versioned with record time (ADR 10).

### Conflicts

For `(subject, predicate)` at `(v, r)`, let `A` be the objects passing steps
1–5, and `S_g` the objects whose group confidence `g_k` from source system
`g` meets the threshold.

| `conflict` | Conflict when | Conflicted objects |
| --- | --- | --- |
| `none` | never | |
| `one` | `A` has more than one object | all of `A` |
| `set` | two systems have non-empty, different `S_g` | objects in `A` not in every non-empty `S_g` |

Objects every system agrees on stay asserted. Resolution, first match:

1. **Manual override** (step 2, an `OverrideSet` event).
2. **Precedence**: a configured, per-predicate ordered list of source
   systems. The first system with a non-empty `S_g` decides: its conflicted
   objects are `asserted`, the others `candidate`.
3. **Authority**: a system is authoritative when a live support in its
   `S_g` is authoritative ([Declarations](#declarations)). If authoritative
   systems have non-empty `S_g` and all those `S_g` are equal, they decide
   as precedence does.
4. Otherwise (two authoritative systems disagree, or none is
   authoritative) the conflict **stands** and its objects are `conflicted`.

Override, precedence and authority decide without opening a conflict; the
disagreement stays queryable, with `resolution` set to the rule
(`override`, `precedence` or `authority`). Conflicts are derived, but
implementations MUST expose them by query and emit `ConflictOpened` and
`ConflictResolved` (with its `resolution`; `evidence_changed` when the
supports stopped disagreeing) for standing conflicts, with audit
records. When valid time passes a known boundary (a `valid_from` or
`valid_to` reached), a scheduler appends a `ValidTimeBoundaryReached` event for it. Applying that
event, like any other, recomputes conflicts for the affected
`(subject, predicate)` and emits the events, so it has a `recorded_at` and
replays. It audits only the conflicts it opens and closes; status changes
caused only by time passing are not audited.

When any non-manual support of `(subject, predicate)` changes after an
override was recorded, the core flags the override `override_stale` for
review (event and audit record).

```json
{ "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "predicate": "owned_by",
  "valid_from": "2026-10-02T10:00:00Z",
  "positions": [ { "source_system": "github",  "authority": { "authoritative": false },
                   "objects": ["0192b1c4-6000-7c5e-a04f-8b3c4d5e6f70"] },
                 { "source_system": "catalog", "authority": { "authoritative": false },
                   "objects": ["0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f"] } ],
  "resolution": null }
```

## Identity across systems

Adapters never decide that keys from two systems are one thing. They report
what their own system records, through declared links and matching fields;
the core turns that into evidence for `same_as` between two subjects of one
kind, whose confidence is the identity **score**. Each evidence group below
counts once in the noisy-OR ([Confidence](#confidence)), so two methods
from one source system both count: a deliberate departure from counting
each source system once.

| Evidence | Support source | ppm |
| --- | --- | --- |
| **Authoritative**: a live `linked_id`, under a link declared (or overridden) authoritative, to an `id`-class key. One system stores another's permanent ID. | `core/identity/link/<source>` | 1000000 |
| Another `linked_id` (not authoritative, or a `name` key resolved through its binding at `observed_at`) | `core/identity/match/link` | configured weight |
| Matching fields | `core/identity/match/<method>` | configured weight |

Evidence follows its inputs on valid time and ends when they stop being
asserted ([Derived claims](#derived-claims)). A link of a key type not in
the kind's declared `links` is rejected (`not_declared`).

### Matching

A **matching value** of subject `S` under method `m` is an asserted fact
`(S, p, x)` at valid time `v` with at least one live support from a source
whose declaration for `(kind(S), p, out)` has `match: m`; for `members`, it
is the set of subjects with asserted `member_of` to `S`, from a field
declared `(kind(S), member_of, in)` with `match: members`. Two subjects
match under `m` when they have matching values under `m`, whatever their
predicates, that are equal under `m`. The matcher scores pairs of active
subjects of one kind that match under any method. A pair the
[merge guard](#merge) excludes (different `id` aliases of one key type in
one namespace) is two things in one system and is not scored.

| Method | Values match when |
| --- | --- |
| `exact` | Equal canonical values (ids, logins). |
| `email` | Equal after Unicode simple case folding of the whole address. |
| `name` | Equal after NFKC, case folding and whitespace collapsing. |
| `members` | Both sets non-empty; the overlap is the size of `A ∩ B` over the size of `A ∪ B`. |

Configuration gives weights in ppm per kind and method, overridden per
source and predicate, plus a weight `link` for non-authoritative links
(`link` is not a declarable `match` method). A matching value's weight is
the highest weight among the fields (declaring `match: m`) of its live
supports; a matching pair contributes the lower of its two values' weights,
for `members` multiplied by the overlap and rounded half to even. Each method's support is the
largest contribution. The weights' default values live in the default
configuration, not in this spec.

### Merge policy

Configuration sets a merge policy per kind. It decides when `same_as` is
`asserted` (step 4d of [Status](#status)) and the two subjects merge:

| Policy | Merges on |
| --- | --- |
| `authoritative` | Live authoritative evidence, or a person's confirmation (`MergeRequested`). |
| `score` | As `authoritative`, or a `same_as` confidence at or above the policy's `threshold_ppm`. |
| `manual` | Only `MergeRequested`. |

For `same_as`, the policy's `threshold_ppm` takes the place of the
predicate threshold. The default configuration sets
`Person: authoritative` and `Team: score`.
A `same_as` the policy doesn't accept stays `candidate` for a person to
confirm. A live `distinct_from` makes `same_as` a `candidate`
(`distinct_from`) and blocks the merge. It opens a conflict on
`(subject, same_as)` at valid times where the authoritative evidence's
covering write has a greater ordering key than the `distinct_from`'s
covering write (step 4a), so the outcome doesn't depend on apply order. `co_reported_ids` and `placeholder` merges don't depend on
the policy. A merge is never undone automatically.

## Manual operations

People act through events on the log (ADR 7), each with an actor and a
reason, applied and audited like any other event. Their source is `manual`.

| Event | Payload | Effect |
| --- | --- | --- |
| `MergeRequested` | `subject_ids` (two), optional `clear_distinct_from` | [Merge](#merge), rule `manual` |
| `UnmergeRequested` | `subject_id`, `aliases` | [Un-merge](#un-merge) |
| `DistinctFromSet` / `DistinctFromCleared` | `subject_ids` (two) | Asserts or ends `distinct_from` |
| `ClaimWithdrawn` | `source`, `subject_id`, `predicate`, `object` | Withdraws that source's claims on the fact |
| `OverrideSet` | `subject_id`, `predicate`, `objects` (fact objects: `{ "subject_id" }` or `{ "type", "value" }`), optional `valid_from`, `valid_to` | A `manual` snapshot of `(subject, predicate)` that decides status (step 2) |
| `OverrideCleared` | `subject_id`, `predicate` | Ends the override at `observed_at` |

## Retention and compaction

Operators configure **tiers** per predicate or per kind in a `Retention`
resource ([ADR 10](../adr/0010-configuration-as-resources.md)); a
predicate's tiers override those of the subject's kind. Each tier keeps a
level of detail for a span of valid time, counted back from the
compaction event's `observed_at`:

```json
{ "predicate": "owned_by", "tiers": [ { "detail": "full", "for": "P18M" }, { "detail": "quarter", "for": "P5Y" }, { "detail": "year" } ] }
```

- `full` keeps every support version. `quarter` and `year` replace the
  history of each `(subject, predicate)` in each UTC calendar quarter or
  year with a **summary**: `{ "period_start", "period_end", "value_at_end",
  "changes", "distinct_values", "compacted_at" }`. `value_at_end` is the
  asserted objects at the last instant before `period_end`, `changes` the
  number of status changes of its facts within the period, and
  `distinct_values` every object asserted at some time in it.
- Only periods entirely past a tier's boundary are compacted. Live
  supports are never compacted. Compaction runs as an event and is audited.
- A summary is Bearing's knowledge at `compacted_at`: reads as recorded
  earlier also answer from it. It stores canonical subject IDs as of
  `compacted_at`, which a later un-merge does not re-point. A later write
  into a compacted period is audited but doesn't change the summary.
- An answer from a summary MUST say so: every `as_of` and `changes` result
  carries `precision: { "detail", "period_start", "period_end" }`, so "who
  owned X in 2024?" answers "Z at year end, after 3 changes".
- The identity store and the audit log are never compacted. Deleting audit
  records is governed by [ADR 8](../adr/0008-audit-log.md)'s retention, not
  by these tiers.

The MVP implements only the `full` tier: nothing is compacted and
`precision.detail` is always `full`.

## Queries

Times default to now.

### As of

`as_of(filter, valid_at, recorded_at)`. The filter takes a `subject_id` or
a key, a predicate, an object and statuses. A key resolves through bindings
valid at `valid_at` as recorded at `recorded_at`; subject IDs canonicalize
through merges recorded by `recorded_at`. Each result has the fact triple,
`status`, `status_reason`, `confidence_ppm`, its valid interval,
`precision`, and the live supports with `source`, `adapter`,
`confidence_ppm`, `observed_at`, valid and record times, `qualifiers` and
`evidence`.

| Question | `valid_at` | `recorded_at` |
| --- | --- | --- |
| Who owns payments-api now? | now | now |
| Who owned it on 1 June, as we know today? | `2026-06-01T00:00:00Z` | now |
| What did Bearing answer on 1 June? | `2026-06-01T00:00:00Z` | `2026-06-01T00:00:00Z` |
| Who is on call now? (`on_call_for`) | now | now |

### What changed

`changes(filter, t1, t2, axis)` is an **endpoint** diff: one entry per fact
whose status or confidence differs between two points, without the steps in
between.

| `axis` | Compares | Answers |
| --- | --- | --- |
| `valid` | `(t1, now)` and `(t2, now)` | What changed in the world, as known now. |
| `record` | `(t1, t1)` and `(t2, t2)` | How Bearing's answers changed, including late and corrected data. |

Facts at both points are matched after canonicalizing subject IDs through
merges as recorded at the later point. With `axis: record`, `from` is
therefore the supports recorded by `t1`, combined under `t2`'s merges.

```json
{ "fact_id": "041d6c02…", "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e",
  "predicate": "owned_by", "object": { "subject_id": "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f" },
  "from": { "status": "asserted", "confidence_ppm": 950000 },
  "to": { "status": "none", "confidence_ppm": 0 }, "precision": { "detail": "full" },
  "supports_changed": ["core/derive/codeowners/github-acme"] }
```

### Data quality

`data_quality(filter, valid_at, recorded_at)` lists issues. The filter
takes kinds, sources and issue types. Each result is
`{ "issue", "subject_ids", "aliases", "supports" }`, the supports with
their `source` and `evidence`:

| `issue` | Listed when |
| --- | --- |
| `unobserved_object` | An object of a relation with a conflict policy, referenced by live supports, that has no live `exists` support (step 5). A placeholder (typically a misspelled team in CODEOWNERS) is the common case; placeholders minted only by `linked_ids` are not listed. |
| `id_conflict` | A `same_as` the [merge guard](#merge) excludes (step 4c). |

A placeholder never counts as an owner: it fails step 5 of
[Status](#status) for predicates with a conflict policy. Relations with
conflict `none` (`member_of`, `on_call_for`) to a placeholder are still
`asserted`.

## Audit

The core writes an audit record ([ADR 8](../adr/0008-audit-log.md)) in the
apply transaction, with the event ID and rule, for: a mint; a binding
written or released; merge, un-merge, `distinct_from`; a fact's status
change caused by an apply; a conflict opened or closed; an override set,
cleared or flagged stale; a compaction or a write into a compacted period;
and every rejection. A rejection rejects its scope; the rest of the event
applies.

| Code | Scope | Raised by |
| --- | --- | --- |
| `kind_mismatch` | observation | [Resolution](#resolution), rule 1; an entity key whose key type is declared for another kind |
| `identity_conflict` | observation or manual event | Resolution rule 1; `MergeRequested` |
| `not_declared` | observation (kind, entity key) or claim (predicate, reference key type, link) | [Declarations](#declarations) |
| `namespace_not_allowed` | observation (entity key) or claim (reference) | [Keys and namespaces](#keys-and-namespaces) |
| `observed_at_in_future` | observation | [observed_at](#observed_at) |
| `duplicate_claim`, `cardinality_mismatch` | observation | [Normalization](#normalization) |
| `domain_mismatch`, `type_mismatch` | claim | [Predicates](#predicates) |
| `core_predicate` | claim | An adapter claims `same_as` or `distinct_from` |
| `invalid_value` | claim | A non-canonical or out-of-range value (NaN, `confidence_ppm` outside `[1, 1000000]`) |
| `invalid_interval` | claim | [Claims](#claims) |
| `already_merged` | manual event | `DistinctFromSet` ([Un-merge](#un-merge)) |
| `invalid_operation` | manual event | An operation its rules don't allow (un-merging a `placeholder` merge, an alias set that isn't a non-empty proper subset) |

## Wire mapping

Protobuf (ADR 6, issue #11) is the only source of truth for these shapes.
JSON Schema and every other format are generated from the `.proto` files,
never written by hand, and there are no hand-written codecs. ProtoJSON uses
proto field names, so JSON field names match the examples here.

- **Core-closed lists are proto enums**: subject status, minting and merge
  rules, key class, `per_subject`, case, merge policy, matching method,
  snapshot direction, cardinality, conflict policy, value type, support
  `reason`, fact status, `status_reason`, conflict `resolution` (`override`,
  `precedence`, `authority`, `evidence_changed`), compaction detail, issue
  types and rejection codes. On the wire and in configuration they are ProtoJSON
  enum names (`FACT_STATUS_ASSERTED`). This document, and `fact_id`, use
  the **short form**: the enum value name without its `<ENUM_NAME>_`
  prefix, lowercased (`asserted`). The CLI MAY print short forms in output
  meant for people.
- Each enum has a zero `*_UNSPECIFIED` value. For a field with a stated
  default it means that default; it is rejected only where the field is
  required.
- **Adapter-declared values are strings**: kinds, predicates, key types and
  namespaces. The core validates them at runtime against the registry here
  and the declarations in force (`not_declared`). Adding kinds or relations
  later is therefore additive.
- Keys are strings in the form `<namespace>:<key_type>/<external_id>`.
- Time fields are `google.protobuf.Timestamp`, truncated to microseconds
  on ingest.
- A typed value is `TypedValue { ValueType type; google.protobuf.Value value; }`.
  A protovalidate CEL rule ties `type` to the value's JSON kind: `string`
  and `time` are strings (`time` in the 6-digit canonical form), `float` a
  number, `bool` a boolean, `json` any JSON value (canonical as JCS). Note:
  with no integer type, `Value` loses nothing.
- Observation attributes are `google.protobuf.Value`, where an explicit
  `null` is meaningful. Everywhere else an absent field equals `null`.
- Confidence is a `uint32 confidence_ppm`. In claim inputs it is optional
  and absent means 1000000; in outputs it is always present (0 is explicit).
- `precision` is `Precision { detail; period_start; period_end; }`.
  Authority is a small `Authority` message holding `bool authoritative`, so
  levels can be added later without breaking declarations or overrides.

## Worked examples

Illustrative IDs; hashes and event IDs shortened; enum values in short
form. Examples use the default configuration and the reference
[declarations](#declarations).

| Name | Subject ID |
| --- | --- |
| R: repo `payments-api` | `0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e` |
| P: team `acme/payments` | `0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f` |
| L: team `acme/platform` | `0192b1c4-6000-7c5e-a04f-8b3c4d5e6f70` |
| J: person jdoe | `0192b2d0-1a01-7e70-b261-ad5e6f708192` |
| G: directory group `payments` | `0192b2d0-1a02-7d6f-a150-9c4d5e6f7081` |

### 1. A repository with a CODEOWNERS team

The [observation above](#observations), from `github-acme`,
`observed_at` 2026-09-28T01:30:00Z, nothing bound yet. CODEOWNERS is the
single line `* @acme/payments`. The same full sync requested the teams page
at 01:29 and observed the team (key `github:team_node/T_kwDOAB12cd`, alias
`github:team/acme/payments`).

1. Both repo keys are unbound: mint R, bind them.
2. The team: if its observation is applied first, it mints P and the repo's
   reference resolves to P. If the repo is applied first, the reference
   mints placeholder P with a tentative binding, and the team observation
   then resolves by name to P, which adopts the node ID. Either way P has
   `exists` from 01:29.
3. Claims: `(R, exists, true)`, `name`, `default_branch`, `language`, and
   `(R, approves_changes, P)` at 1000000. `topics: []` and
   `description: null` record that R has none.
4. The file is one `*` line naming one team, so `codeowners` derives
   `(R, owned_by, P)` at 950000 from `core/derive/codeowners/github-acme`:
   `asserted` from 01:30. Until the team observation is applied, reads give
   `candidate` (`unobserved_object`); the final valid-time state doesn't
   depend on the order. Had the file also had path rules, `owned_by` would
   be 700000, a `candidate`.
5. The scope `(github-acme, R, out, [approves_changes])` has watermark key
   `(2026-09-28T01:30:00Z, "github:repo_node/R_kgDOH1a2b3@…", "github-acme/…", <hash>)`.

### 2. Repository rename

`acme/payments-api` becomes `acme/payments` at 2026-10-01T12:00:00Z:

```json
{ "id": "github:repo_node/R_kgDOH1a2b3@2026-10-01T12:00:00.000000Z", "time": "2026-10-01T12:00:00Z",
  "data": { "entity": { "kind": "Repository", "key": "github:repo_node/R_kgDOH1a2b3",
                        "aliases": ["github:repo/acme/payments"], "attributes": { "name": "payments" } } } }
```

- The node ID resolves to R.
- `github:repo/acme/payments` maps to R from 12:00; `per_subject: one`
  releases `github:repo/acme/payments-api` from 12:00. It redirects, so
  references to the old name still resolve to R.
- `name` is `one`: `"payments-api"` ends at 12:00, `"payments"` starts.
- `(R, approves_changes, P)` and the derived `owned_by` are untouched.

### 3. A team removed from CODEOWNERS

A full sync requests R's CODEOWNERS at 2026-10-02T09:00:00Z; it is now the
single line `* @acme/platform`:

```json
{ "id": "github:repo_node/R_kgDOH1a2b3@2026-10-02T09:00:00.000000Z", "time": "2026-10-02T09:00:00Z",
  "data": { "entity": { "kind": "Repository", "key": "github:repo_node/R_kgDOH1a2b3",
                        "aliases": ["github:repo/acme/payments"], "attributes": { "codeowners_rules": 1 } },
            "relations": [ { "type": "approves_changes", "to": "github:team/acme/platform",
                             "attributes": { "pattern": "*", "file": ".github/CODEOWNERS", "line": 1 } } ],
            "snapshots": [ { "direction": "out", "predicates": ["approves_changes"] } ] } }
```

- `(R, approves_changes, L)` is claimed from 09:00, and the watermark ends
  `(R, approves_changes, P)` there. Its support versions:

  | valid_from | valid_to | recorded_at | retracted_at | reason |
  | --- | --- | --- | --- | --- |
  | 2026-09-28T01:30:00Z | null | 2026-09-28T01:30:02Z | 2026-10-02T09:00:03Z | assert |
  | 2026-09-28T01:30:00Z | 2026-10-02T09:00:00Z | 2026-10-02T09:00:03Z | null | snapshot |

- The derived `(R, owned_by, P)` ends with it (reason `derived`):
  `asserted` on `[2026-09-28T01:30:00Z, 2026-10-02T09:00:00Z)`, `none`
  after. `(R, owned_by, L)` is asserted at 950000 from 09:00 (L was
  observed earlier).

A webhook observed at 08:55 that still lists P, applied late, changes
nothing: at valid times from 09:00 the watermark's key is greater.

### 4. A directory group membership with an end date

Source `authentik-acme` reports group G and its members; jdoe's
membership ends on 1 November, and the directory records its start (a
backfilled `valid_from`):

```json
{ "id": "authentik:group/5b0e…@2026-10-02T06:00:00.000000Z", "time": "2026-10-02T06:00:00Z",
  "data": { "entity": { "kind": "Team", "key": "authentik:group/5b0e…",
                        "aliases": ["authentik:group_name/payments"], "attributes": { "name": "payments" } },
            "relations": [ { "type": "member_of", "from": "authentik:user/7f3c…",
                             "valid_from": "2026-03-01T00:00:00Z", "valid_to": "2026-11-01T00:00:00Z" } ],
            "snapshots": [ { "direction": "in", "predicates": ["member_of"] } ] } }
```

- The Authentik user is J once the person merge in example 7 is applied.
- `(J, member_of, G)` is asserted on `[2026-03-01, 2026-11-01)` and ended
  from 2026-11-01. `as_of(valid_at: 2026-10-15T00:00:00Z)` gives
  `asserted`; `as_of(valid_at: 2026-11-02T00:00:00Z)` gives `none`, with no
  further event.
- A later claim extending the end to 2026-12-01 has a greater key and wins
  on `[2026-11-01, 2026-12-01)`.

### 5. A conflicting ownership claim

A catalog source (`catalog-acme`, system `catalog`) observes at
2026-10-02T10:00:00Z that R is owned by P at 1000000; its adapter declares
`owned_by` on `Repository` authoritative. GitHub's derived support says L
at 950000.

- `owned_by` is `set`: `S_github = {L}`, `S_catalog = {P}`. Only `catalog`
  is authoritative, so it decides: P is `asserted` and L `candidate`
  (`authority`). No conflict opens.
- If the operator overrides `catalog-acme`'s authority for `owned_by` to
  `false`, neither system is authoritative: a conflict opens from 10:00
  ([shape](#conflicts)), both facts are `conflicted`, and "who owns R?"
  returns no asserted owner, plus the conflict.
- Precedence `owned_by: [github, catalog]` would make L `asserted` and P
  `candidate` (`precedence`), even with catalog authoritative. Or
  `OverrideSet { subject_id: R, predicate: owned_by, objects: [L] }`: L
  `asserted`, P `overridden`. Both are audited.

### 6. An "as of" query

After examples 1–3, at 2026-10-02T12:00:00Z:

```json
{ "filter": { "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "predicate": "owned_by",
              "status": ["asserted"] },
  "valid_at": "2026-10-01T00:00:00Z", "recorded_at": "2026-10-02T12:00:00Z" }
```

```json
{ "facts": [ {
    "fact_id": "041d6c02…", "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e",
    "predicate": "owned_by", "object": { "subject_id": "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f" },
    "status": "asserted", "status_reason": "none", "confidence_ppm": 950000,
    "precision": { "detail": "full" },
    "valid_from": "2026-09-28T01:30:00Z", "valid_to": "2026-10-02T09:00:00Z",
    "supports": [ { "source": "core/derive/codeowners/github-acme", "adapter": null, "confidence_ppm": 950000,
                    "observed_at": "2026-09-28T01:30:00Z",
                    "valid_from": "2026-09-28T01:30:00Z", "valid_to": "2026-10-02T09:00:00Z",
                    "recorded_at": "2026-10-02T09:00:03Z", "retracted_at": null, "qualifiers": [],
                    "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" } } ] } ] }
```

With `valid_at` now it returns L. With both times 2026-10-01T00:00:00Z it
returns P with `valid_to: null`: on 1 October Bearing didn't yet know the
ownership would end.

### 7. Matching across GitHub and the directory

- jdoe's Authentik record links their GitHub node ID
  (`linked_ids: ["github:user_node/U_kgDOA1b2c3"]`): authoritative evidence
  at 1000000, so the Authentik person and J merge (rule `authoritative`; the
  earlier mint survives).
  A matching email alone would leave a `candidate` to confirm.
- P and G match on `name` and, once their members have merged, on
  `members`. At or above the `Team` threshold they merge (rule `score`);
  below it `same_as` stays a `candidate`.

## Open questions

For Doug:
1. **Enum spelling in JSON.** ProtoJSON writes `FACT_STATUS_ASSERTED`, not
   `asserted`, in APIs and configuration. *Recommendation:* accept the
   ProtoJSON names on the wire and in config, with no hand-written codecs
   (ADR 6), short forms in prose and `fact_id`, and short forms in the
   CLI's output for people.

## Follow-ups

To do once this is approved, in the M1 implementation. The scaffold
artifacts are replaced, not migrated:

- `proto/bearing/model/`: the data model, events and declarations as
  Protobuf, with JSON Schema generated from it. Declarations are fields of
  `DescribeResponse` in `proto/bearing/adapter/v1` (issue #11). ADR 6 says
  `v1` packages, issue #11 says `v1alpha1`: reconcile.
- Delete `schema/observation.v1.schema.json`, the hand-written types in
  `pkg/model`, `testdata/observations.ndjson` and the `bearing validate`
  command (and its row in `AGENTS.md`'s commands table); replace them with
  generated types plus small helpers (key parsing, `fact_id`) and new
  example observations that follow this spec.
- The default configuration: match weights per kind and method, the `link`
  weight, the `Team` merge threshold, the `codeowners` rule's ppm per shape.
- `adapters/github`: node IDs as keys with the `X-Github-Next-Global-ID: 1`
  header, names as aliases, `full_name`; the effective CODEOWNERS file as
  `approves_changes` with line qualifiers, `codeowners_rules` and snapshot
  scopes; the reference declaration; send `null`/`[]` for empty
  attributes; `complete_sync` only after it moves to cursor (GraphQL)
  paging; `observed_at` per the rules above.
- `pkg/adapter`: the declaration shape in `Describe`, `complete_sync` on
  the last page, the source `namespace`, `issues` and `links` fields; bump
  `adapter.ProtocolVersion`. Reconcile with ADR 9 and PR #26, which retire
  the stdio transport for a Protobuf adapter service.
- `docs/spec/contracts.md` and `pkg/contracts`: entities become subjects;
  `GraphStore` gains resolution, merge, un-merge, `Apply`, `as_of`,
  `changes`, `data_quality` and conflicts; vector points re-point on merge;
  `EventBus` superseded by ADR 7's `EventLog`, with a home for the new event
  types; conformance tests and shared test vectors for every rule here,
  a backup-and-restore conformance test for primary state,
  including apply-order independence and merge-trigger order.
- ADR 7: partition key (`(source, key)` vs subject), event IDs that include
  the source, and the new event types (manual operations,
  `ValidTimeBoundaryReached`, derived deletions, compaction, and a
  declaration change when a source's adapter is upgraded).
- ADR 8: reconcile its "what gets recorded" list and retention with
  [Audit](#audit) and [Retention and compaction](#retention-and-compaction).
- ADR 10: `namespace`, `issues` (with `key_classes`), `links`, `authority`,
  `confidence_groups`, match weights, merge policies, derivation rules and
  the `Retention` resource's tiers as configuration; reconcile its settings
  descriptor in `Describe` with `config_schema`.
- `docs/telemetry.md`: spans and metrics for apply, matching, merges,
  conflicts and compaction.
- Issue #27: the M3 apply-clock load benchmark against SurrealDB.
- `AGENTS.md`, `README.md` and the `new-adapter` skill: declarations, keys,
  aliases, snapshots and the "send `null` for empty" rule in the adapter
  checklist.
