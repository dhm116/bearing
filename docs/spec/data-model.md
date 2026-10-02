# Data model

Version 0.2 (draft). The rule sections (from [State](#state-determinism-and-apply)
to [Wire mapping](#wire-mapping)) are normative: statements there in the
imperative or present tense ("is", "becomes", "writes") are requirements,
as if they said MUST. Worked examples and open questions are not.

## Changes in this revision

v0.2 makes exact what v0.1 left open: how keys become things, how history
works and what wins when sources disagree.

| Change | Why |
| --- | --- |
| A **subject** with a Bearing-minted, never-reused ID is what everything attaches to. Source keys are its **aliases**, `id` (immutable) or `name` (time-bounded). Replaces v0.1 Keys. | Renames and transfers keep the subject; reused names get a new one. |
| Exact resolution, minting, merge and un-merge rules, audited. The identity store is primary state. | Identity is where implementations would diverge. |
| **Facts** `(subject, predicate, object)`; attributes are facts; relation types join a predicate registry. Replaces v0.1 Facts. | One model with provenance, time and confidence for everything. |
| **Bitemporal**: claims write states onto valid time; supports have record time. "As of" and "what changed" queries. | Time-bounded facts are core, not special cases. On-call is just one enrichment. |
| Per-source supports, latest `observed_at` wins per valid time, snapshot scopes, sync completeness. | A stale sync can't undo a newer event; endings are explicit. |
| Confidence in parts per million: max within a source system, noisy-OR across. Statuses and conflicts. | Corroboration counts once per independent system; disputed owners are never asserted. |
| Person identity across GitHub and the directory. | The MVP sources are GitHub and Authentik. |

**Compatibility.** The observation type stays `dev.bearing.observation.v1`
and the new fields are optional, but v0.2 is additive **except**: a
reference with an unregistered key type and a multi-kind range is rejected
(`on_call_for` to `pagerduty:service/…` in `testdata/`); attribute `null`
ends a claim and an array is the complete set; unregistered attributes are
renamed `<namespace>.<attribute>`; a relation may carry `from` instead of
`to`; `Change.author` and `Schedule.on_call_now` are dropped. Accepted
because the spec is pre-1.0 and the only consumers are in this repository.

## Terms

| Term | Meaning |
| --- | --- |
| Subject | A thing Bearing knows about, identified by a `subject_id`. |
| Kind | A subject's type (`Person`, `Repository`, …), fixed at mint. |
| Issuer type | A kind of identifier issuer with registered key types: `github`, `authentik`, `saml`. |
| Namespace | One issuer instance, for example `github` (github.com) or `ghes-acme`. Each has an issuer type. |
| Alias | `(namespace, key_type, external_id)`, written as a key `<namespace>:<key_type>/<external_id>`. |
| Source | A configured adapter instance ([ADR 10](../adr/0010-configuration-as-resources.md)), for example `github-acme`. The unit of provenance. |
| Source system | The namespace a source reads. Sources of one system are not independent. |
| Observation | One source's report about one entity at one `observed_at`. |
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
- The **claim store**: claims, watermarks, support versions and audit
  links. Replaying observations against the identity store rebuilds it.

Guarantees:

1. Given the same identity decisions (the same mints, bindings, merges and
   rejections, for example by replaying against the final identity store)
   and the same set of events, conforming implementations produce the same
   valid-time state: supports, fact statuses, confidence and conflicts at
   every valid time, **regardless of the order** the events are applied in.
   (The event log orders events only within a partition.)
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
  This is the only isolation model in v0.2.

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

UUIDv7 is a standard with native database types, and its mint-time order
gives merges a deterministic survivor. A subject with no `exists` claim ever
recorded (only referenced) is a **placeholder**.

## Identifiers and aliases

*Replaces v0.1 "Keys". The key syntax is unchanged.*

### Keys and namespaces

```
<namespace>:<key_type>/<external_id>
github:repo_node/R_kgDOH1a2b3
authentik:user/7f3c2a9e-1b4d-4c8e-9a0f-2d6e8b1c5a74
```

`namespace` and `key_type` are lowercase letters, digits and hyphens
(`key_type` also allows `_`). `external_id` is the issuer's value and MAY
contain `/`.

Namespaces come from source configuration:

| Source config field | Meaning |
| --- | --- |
| `namespace` | The namespace the source reads. Default: its adapter's issuer type (`github` for github.com). A GitHub Enterprise Server source sets e.g. `ghes-acme`. |
| `issues` | Other namespaces the source is the issuer for: `[{ "namespace": "authentik-saml", "issuer_type": "saml", "nameid_format": "persistent" }]`. |
| `links` | Namespaces the source reports `linked_ids` in, with their issuer types. |

An adapter declares its issuer type, which is the default `namespace`. A key
in a namespace the source neither reads, issues nor links is rejected
(`namespace_not_allowed`).

Every configured use of a namespace MUST name the same issuer type;
configuration apply rejects a disagreement. Key types are looked up by the
namespace's issuer type.

### Key types

| Property | Meaning |
| --- | --- |
| `kind` | Kind of subjects with this key type; used to mint placeholders. |
| `class` | `id`: assigned once, never changed or reassigned. `name`: can be renamed and later reused. |
| `per_subject` | `name` only. `one`: binding a new name of this type to a subject releases its previous one. |
| `redirects` | `name` only. A released name still resolves to its last subject (as GitHub does for repositories). |
| `case` | `insensitive` external IDs are compared and stored after Unicode simple case folding. |
| `link_rules` | Rules allowed when this key type appears in `linked_ids`. |

| Issuer type | Key type | Kind | Class | Per subject | Redirects | Case | Link rules |
| --- | --- | --- | --- | --- | --- | --- | --- |
| github | `repo_node` | Repository | id | | | sensitive | |
| github | `repo` | Repository | name | one | yes | insensitive | |
| github | `team_node` | Team | id | | | sensitive | |
| github | `team` | Team | name | one | no | insensitive | |
| github | `user_node` | Person | id | | | sensitive | `directory_link` |
| github | `user` | Person | name | one | no | insensitive | `directory_link_login` |
| authentik | `user` | Person | id | | | sensitive | |
| authentik | `username` | Person | name | one | no | insensitive | |
| authentik | `group` | Team | id | | | sensitive | |
| authentik | `group_name` | Team | name | one | no | sensitive | |
| saml | `name_id` | Person | `id` if the namespace's `nameid_format` is `persistent`, else `name` | one (if `name`) | no | sensitive | `saml_name_id` |

- GitHub `id` keys are next-format global node IDs (the adapter sends
  `X-Github-Next-Global-ID: 1`). Legacy node IDs MUST NOT be emitted. Node
  IDs survive renames, transfers and login changes.
- Directory `id` keys are the directory's immutable primary keys.
- **Unregistered key types** are class `id`, case-sensitive. Their kind is
  the entity's `kind` when used as an entity key or alias, or the
  predicate's range when used in a reference and the range is one kind.
  Otherwise the claim is rejected (`unknown_key_type`). The first use fixes
  the kind, recorded in the identity store; a later use with another kind is
  rejected (`kind_mismatch`).

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
  **tentative**. It counts wherever the name is unbound or released without
  redirect; an observed binding to a subject overrides it.

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
`redirects`. Otherwise, and for an unbound key, a placeholder is minted
(rule `reference`) with a tentative binding, so later references to the
same name reuse it.

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
| `identity` | `same_as` between them becomes `asserted`. | computed |
| `manual` | A `MergeRequested` event. Rejected (`identity_conflict`) while a `distinct_from` between them is live, unless the request also clears it. | 1.0 |

Only subjects of the same kind merge. At record time `r`:

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
   records joining the subjects its `via` aliases were bound to. If an
   `identity` merge's evidence falls below threshold, the core opens a
   conflict on `(subject, same_as)` for review. It never un-merges by itself.
5. Reads as recorded before `r` still show two subjects.
6. An audit record holds the rule, confidence, evidence, both alias sets and
   the event ID.

### Un-merge

Only an `UnmergeRequested` event un-merges. It names an active subject `A`
and a non-empty proper subset `D` of `A`'s aliases. At record time `r`:

1. If `D` is the alias set some `B` had when it merged into `A`, `B` becomes
   `active` again; subjects earlier merged into `B` stay merged into `B`.
   Otherwise mint a subject (rule `split`).
2. Binding writes of aliases in `D` that map them to `A` are re-pointed to
   it, for all valid time.
3. A claim whose `via` set is within `D` gets a re-pointing record (stored
   claims are not rewritten) and counts for it. Claims whose `via` spans
   both sides, or that have none (manual, core), stay with `A` and are
   listed in the audit record for review.
4. A `distinct_from` between the two is set (source `manual`).

A `DistinctFromSet` for two subjects already merged is rejected
(`already_merged`); un-merge instead.

## Subject kinds

*Replaces v0.1 "Entity kinds". The kinds are unchanged.*

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

`Change.author` is now the `changed_by` fact. `Schedule.on_call_now` is gone:
an on-call source reports `on_call_for` facts with `valid_from`/`valid_to`
per shift, and "who is on call now" is an [as-of query](#as-of).

## Predicates

*Replaces v0.1 "Relation types".*

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
- `verified_email`: an address the source verified on a domain the
  organization verified. `commit_email`: an author address of commits the
  source links to the person.
- An unregistered attribute becomes `<namespace>.<attr>` (namespace of the
  source system), cardinality `one` for a scalar and `many` for an array,
  conflict `none`, type from the JSON: string, number → `float`, boolean,
  object → `json`.

**Core predicates** (adapters MUST NOT claim them): `same_as` and
`distinct_from`, Person → Person or Team → Team. Both are symmetric and
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

*Extends v0.1.* Example (envelope fields `specversion`, `type`,
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
                      "topics": [], "description": null }
    },
    "relations": [
      { "type": "owned_by", "to": "github:team/acme/payments",
        "attributes": { "pattern": "*", "file": ".github/CODEOWNERS" } }
    ],
    "snapshots": [ { "direction": "out", "predicates": ["owned_by"] } ],
    "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" }
  }
}
```

| Field | Rule |
| --- | --- |
| `time` | `observed_at` of every claim; see below. |
| `entity.key` | Primary key; SHOULD be `id` class when the source has one. |
| `entity.aliases` | More keys for the entity, only in the source's `namespace` or its `issues` namespaces. |
| `entity.linked_ids` | Keys in the source's `links` namespaces that the source records for the entity: `[{ "key", "rule" }]`. Identity evidence, never an alias. When present, the list is complete for this source. |
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
| `linked_ids` | evidence for `(E, same_as, target)` from source `core/identity/<rule>/<source>` | the source's linked-id evidence for `E` |

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
  source and is missing from **two consecutive** complete syncs is treated
  as `entity.deleted` at the first missing sync's `t0`, with watermark key
  `(t0, <sync ID>, <sync ID>, "")`.
- The core MAY append one derived deletion event per subject rather than
  apply them all in one transaction.

## Facts and supports

*Replaces v0.1 "Facts".*

### Facts

```json
{ "fact_id": "041d6c02c06fa8bf4c8d093d9a3dce979580c4d9dfb7ead9b89542451890be89",
  "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "predicate": "owned_by",
  "object": { "subject_id": "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f" } }
```

- `object` is `{ "subject_id" }` or `{ "type", "value" }` (value in
  canonical form).
- `fact_id` is the lowercase hex SHA-256 of the JCS form of
  `{ "subject_id", "predicate", "object" }`, with canonical (survivor) IDs.
  Test vectors:

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
{ "fact_id": "041d6c02…", "source": "github-acme", "adapter": "github@0.2.0",
  "event_id": "github-acme/9f2c…", "observation_id": "github:repo_node/R_kgDOH1a2b3@2026-09-28T01:30:00.000000Z",
  "observed_at": "2026-09-28T01:30:00Z", "last_confirmed_at": "2026-10-01T06:00:00Z",
  "confidence_ppm": 1000000, "valid_from": "2026-09-28T01:30:00Z", "valid_to": null,
  "recorded_at": "2026-09-28T01:30:02Z", "retracted_at": null, "reason": "assert",
  "via": { "subject": ["github:repo_node/R_kgDOH1a2b3", "github:repo/acme/payments-api"],
           "object": "github:team/acme/payments" },
  "qualifiers": [ { "file": ".github/CODEOWNERS", "pattern": "*" } ],
  "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" } }
```

- One row per maximal valid-time interval with one state, confidence and
  qualifier set. `adapter` is the adapter name and version that produced
  the observation.
- Rows are immutable except that `retracted_at` is set once, to the
  `recorded_at` of the apply that replaces them.
- `reason`: `assert`, `end`, `snapshot`, `deleted`, `withdrawn`, `merge`,
  `unmerge`.
- A materialized store MUST keep the ordering key per valid-time segment. A
  **confirming** claim (same state, later key) replaces the keys on
  `[observed_at, ∞)` only, and creates no new support version.
  `last_confirmed_at` is informational.

Both forms give the same answers to every query here.

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
`content_hash` = SHA-256 of the JCS form of the observation's `data`. All
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

The 10:00 page may come from a lagging replica of the source. Bearing can't
detect that; the next sync corrects it.

## Confidence

All confidence is integer **parts per million** (ppm). At `(v, r)`:

1. Group the fact's live supports by **source system**. `manual` is one
   group; each `core/identity/<rule>` is one group. Systems listed together
   in the configuration field `confidence_groups` (systems that copy each
   other) form one group.
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
| 4 | Relation predicate with `conflict` ≠ `none`, and the object has no live `exists` support | `candidate` | `unobserved_object` |
| 5a | In a conflict, with precedence configured | `asserted` or `candidate` | `precedence` |
| 5b | In a conflict, no precedence | `conflicted` | `conflict` |
| 6 | Otherwise | `asserted` | |

Answers that act (ownership, policy) read only `asserted` facts
([ADR 2](../adr/0002-graph-is-source-of-truth.md)).

A fact's reported `valid_from`/`valid_to` at `(v, r)` is the maximal
interval containing `v` over which its status, as recorded at `r`, is
unchanged.

Thresholds, precedence, groups and identity rule confidences are those of
the configuration in force at record time `r`. Configuration changes are
versioned with record time (ADR 10).

### Conflicts

For `(subject, predicate)` at `(v, r)`, let `A` be the objects passing steps
1–4, and `S_g` the objects whose group confidence `g_k` from source system
`g` meets the threshold.

| `conflict` | Conflict when | Conflicted objects |
| --- | --- | --- |
| `none` | never | |
| `one` | `A` has more than one object | all of `A` |
| `set` | two systems have non-empty, different `S_g` | objects in `A` not in every non-empty `S_g` |

Objects every system agrees on stay asserted. Resolution:

- **Manual override** (step 2, an `OverrideSet` event).
- **Precedence**: a configured, per-predicate ordered list of source
  systems. The first system with a non-empty `S_g` decides: its conflicted
  objects are `asserted`, the others `candidate`.

Conflicts are derived, but implementations MUST expose them by query and
emit `ConflictOpened` / `ConflictResolved` events, with audit records.
When valid time passes a known boundary (a `valid_to` reached), a
scheduler appends a `ValidTimeBoundaryReached` event for it. Applying that
event, like any other, recomputes conflicts for the affected
`(subject, predicate)`, emits the events and writes the audit records, so it
has a `recorded_at` and replays. Other status changes caused only by time
passing are not audited.

When any non-manual support of `(subject, predicate)` changes after an
override was recorded, the core flags the override `override_stale` for
review (event and audit record).

```json
{ "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "predicate": "owned_by",
  "valid_from": "2026-10-02T10:00:00Z",
  "positions": [ { "source_system": "github",  "objects": ["0192b1c4-6000-7c5e-a04f-8b3c4d5e6f70"] },
                 { "source_system": "catalog", "objects": ["0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f"] } ],
  "resolution": null }
```

## Identity across systems

Adapters never decide that keys from two systems are one thing. They report
what their own system records; the core turns that into `same_as` evidence
and merges when `same_as` is asserted.

| Rule | Evidence | ppm | Strength |
| --- | --- | --- | --- |
| `directory_link` | The directory stores the GitHub user's node ID (`linked_ids`, `github:user_node`). | 1000000 | strong |
| `saml_name_id` | GitHub's linked SAML identity reports the NameID (`linked_ids`); the directory issues it as an alias. | 1000000 if the namespace is `persistent`, else 850000 | strong if persistent, else weak |
| `manual` | `MergeRequested`. | 1000000 | strong |
| `directory_link_login` | The directory stores a GitHub login, resolved through the `github:user` binding at `observed_at`. Logins are reusable. | 850000 | weak |
| `verified_domain_email` | A GitHub `verified_email` equals a directory `email`. | 800000 | weak |
| `commit_email` | A GitHub `commit_email` equals a directory `email`. | 600000 | weak |
| `display_name` | Equal `name` after NFKC, case folding and whitespace collapsing. | 300000 | weak |

- Emails are compared after Unicode simple case folding of the whole
  address.
- Evidence sources are `core/identity/<rule>/<origin>`: the reporting
  source for `linked_ids` rules, `core` for the matching rules, which the
  core re-evaluates whenever their input facts change and ends when the
  inputs stop being asserted.
- A `linked_ids` rule not in the key type's `link_rules`, or a reserved rule
  (`manual` and the matching rules), is rejected (`unknown_rule`).
- `same_as` combines as in [Confidence](#confidence); **if no strong rule
  is live, it is capped at 850000**, below the default threshold. Weak
  evidence never merges; it yields a `candidate` for a person to confirm.
- A live `distinct_from` blocks the merge and opens a conflict on
  `(subject, same_as)` (the [conflict shape](#conflicts), `ConflictOpened`).
- Teams and groups are not matched automatically.

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

## Queries

Times default to now.

### As of

`as_of(filter, valid_at, recorded_at)`. The filter takes a `subject_id` or
a key, a predicate, an object and statuses. A key resolves through bindings
valid at `valid_at` as recorded at `recorded_at`; subject IDs canonicalize
through merges recorded by `recorded_at`. Each result has the fact triple,
`status`, `status_reason`, `confidence_ppm`, its valid interval, and the
live supports with `source`, `adapter`, `confidence_ppm`, `observed_at`,
valid and record times, `qualifiers` and `evidence`.

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
  "from": { "status": "asserted", "confidence_ppm": 1000000 },
  "to": { "status": "none", "confidence_ppm": 0 }, "supports_changed": ["github-acme"] }
```

## Audit

The core writes an audit record ([ADR 8](../adr/0008-audit-log.md)) in the
apply transaction, with the event ID and rule, for: a mint; a binding
written or released; merge, un-merge, `distinct_from`; a fact's status
change caused by an apply; a conflict opened or closed; an override set,
cleared or flagged stale; and every rejection (`kind_mismatch`,
`type_mismatch`, `domain_mismatch`, `unknown_key_type`, `unknown_rule`,
`duplicate_claim`, `cardinality_mismatch`, `invalid_interval`,
`identity_conflict`, `already_merged`, `namespace_not_allowed`,
`observed_at_in_future`).

## Wire mapping

The Protobuf model (ADR 6, issue #11) encodes this document; ProtoJSON uses
proto field names, so JSON matches the examples here.

- Enumerated values (kinds, statuses, reasons, classes, directions) are
  strings checked by protovalidate `in` rules, not proto enums, so they
  appear exactly as written here (`"Person"`, `"asserted"`) and v1 JSON
  stays valid.
- A typed value is `TypedValue { string type; google.protobuf.Value value; }`,
  so its ProtoJSON is `{ "type", "value" }`. A protovalidate CEL rule ties
  `type` to the value's JSON kind: `string` and `time` are strings (`time`
  in the canonical form), `float` a number, `bool` a boolean, `json` an
  object. With no integer type, `Value` loses nothing.
- Observation attributes are `google.protobuf.Value`, where an explicit
  `null` is meaningful. Everywhere else an absent field equals `null`.
- Confidence is an optional `uint32 confidence_ppm`; absent means 1000000.

## Worked examples

Illustrative IDs; hashes and event IDs shortened.

| Name | Subject ID |
| --- | --- |
| R: repo `payments-api` | `0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e` |
| P: team `acme/payments` | `0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f` |
| L: team `acme/platform` | `0192b1c4-6000-7c5e-a04f-8b3c4d5e6f70` |
| J: person jdoe | `0192b2d0-1a01-7e70-b261-ad5e6f708192` |
| G: directory group `payments` | `0192b2d0-1a02-7d6f-a150-9c4d5e6f7081` |

### 1. A repository with a CODEOWNERS team owner

The [observation above](#observations), from `github-acme`,
`observed_at` 2026-09-28T01:30:00Z, nothing bound yet. The same full sync
requested the teams page at 01:29 and observed the team (key
`github:team_node/T_kwDOAB12cd`, alias `github:team/acme/payments`).

1. Both repo keys are unbound: mint R, bind them.
2. The team: if its observation is applied first, it mints P and the repo's
   reference resolves to P. If the repo is applied first, the reference
   mints placeholder P with a tentative binding, and the team observation
   then resolves by name to P, which adopts the node ID. Either way P has
   `exists` from 01:29.
3. Claims: `(R, exists, true)`, `name`, `default_branch`, `language`, and
   `(R, owned_by, P)`. `topics: []` and `description: null` record that R
   has none.
4. `(R, owned_by, P)` is `asserted` from 01:30. Until the team observation
   is applied, reads give `candidate` (`unobserved_object`); the final
   valid-time state doesn't depend on the order.
5. The scope `(github-acme, R, out, [owned_by])` has watermark key
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
- `(R, owned_by, P)` is untouched.

### 3. A team removed from CODEOWNERS

A full sync requests R's CODEOWNERS at 2026-10-02T09:00:00Z; it lists only
`@acme/platform`:

```json
{ "id": "github:repo_node/R_kgDOH1a2b3@2026-10-02T09:00:00.000000Z", "time": "2026-10-02T09:00:00Z",
  "data": { "entity": { "kind": "Repository", "key": "github:repo_node/R_kgDOH1a2b3",
                        "aliases": ["github:repo/acme/payments"] },
            "relations": [ { "type": "owned_by", "to": "github:team/acme/platform",
                             "attributes": { "pattern": "*", "file": ".github/CODEOWNERS" } } ],
            "snapshots": [ { "direction": "out", "predicates": ["owned_by"] } ] } }
```

- `(R, owned_by, L)` is claimed from 09:00 (L was observed earlier, so it
  is asserted).
- The watermark ends `(R, owned_by, P)` at 09:00. It had no other support:
  `asserted` on `[2026-09-28T01:30:00Z, 2026-10-02T09:00:00Z)`, `none` after.

| valid_from | valid_to | recorded_at | retracted_at | reason |
| --- | --- | --- | --- | --- |
| 2026-09-28T01:30:00Z | null | 2026-09-28T01:30:02Z | 2026-10-02T09:00:03Z | assert |
| 2026-09-28T01:30:00Z | 2026-10-02T09:00:00Z | 2026-10-02T09:00:03Z | null | snapshot |

A webhook observed at 08:55 that still lists P, applied late, changes
nothing: at valid times from 09:00 the watermark's key is greater.

### 4. A directory group membership with an end date

Source `authentik-acme` reports group G and its members; jdoe's
membership ends on 1 November (the directory holds an end date):

```json
{ "id": "authentik:group/5b0e…@2026-10-02T06:00:00.000000Z", "time": "2026-10-02T06:00:00Z",
  "data": { "entity": { "kind": "Team", "key": "authentik:group/5b0e…",
                        "aliases": ["authentik:group_name/payments"], "attributes": { "name": "payments" } },
            "relations": [ { "type": "member_of", "from": "authentik:user/7f3c…",
                             "valid_from": "2026-03-01T00:00:00Z", "valid_to": "2026-11-01T00:00:00Z" } ],
            "snapshots": [ { "direction": "in", "predicates": ["member_of"] } ] } }
```

- `(J, member_of, G)` is asserted on `[2026-03-01, 2026-11-01)` and ended
  from 2026-11-01. `as_of(valid_at: 2026-10-15T00:00:00Z)` gives
  `asserted`; `as_of(valid_at: 2026-11-02T00:00:00Z)` gives `none`, with no
  further event.
- A later claim extending the end to 2026-12-01 has a greater key and wins
  on `[2026-11-01, 2026-12-01)`.

### 5. A conflicting ownership claim

A catalog source (`catalog-acme`, system `catalog`) observes at
2026-10-02T10:00:00Z that R is owned by P; GitHub says L. Both 1000000 ppm.

- `owned_by` is `set`: `S_github = {L}`, `S_catalog = {P}`. A conflict
  opens from 10:00 ([shape](#conflicts)); both facts are `conflicted`.
- "Who owns R?" returns no asserted owner, plus the conflict.
- With precedence `owned_by: [catalog, github]`, P is `asserted` and L
  `candidate` (`precedence`).
- Or `OverrideSet { subject_id: R, predicate: owned_by, objects: [L] }`: L
  `asserted`, P `overridden`. Both are audited and the conflict closes.

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
    "status": "asserted", "status_reason": null, "confidence_ppm": 1000000,
    "valid_from": "2026-09-28T01:30:00Z", "valid_to": "2026-10-02T09:00:00Z",
    "supports": [ { "source": "github-acme", "adapter": "github@0.2.0", "confidence_ppm": 1000000,
                    "observed_at": "2026-09-28T01:30:00Z",
                    "valid_from": "2026-09-28T01:30:00Z", "valid_to": "2026-10-02T09:00:00Z",
                    "recorded_at": "2026-10-02T09:00:03Z", "retracted_at": null,
                    "qualifiers": [ { "file": ".github/CODEOWNERS", "pattern": "*" } ],
                    "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" } } ] } ] }
```

With `valid_at` now it returns L. With both times 2026-10-01T00:00:00Z it
returns P with `valid_to: null`: on 1 October Bearing didn't yet know the
ownership would end.

## Open questions

For Doug:

1. **Identity store as primary state.** Subjects, bindings and merges can't
   be rebuilt from a windowed, partition-ordered log, so v0.2 makes them
   primary state, backed up with the graph. This narrows ADR 2's
   "rebuildable" story to facts and the vector index. *Recommendation:*
   accept, and amend ADR 2 and ADR 7 to say so.
2. **Directory groups as Teams.** Should GitHub teams link to directory
   groups automatically when GitHub team sync ties them, or only by hand?
3. **Verified domain email** (800000, weak). Strong for orgs without SAML?
4. **Login links.** `directory_link_login` is weak (850000) because logins
   are reusable. Make it strong when the login's binding hasn't changed
   since the directory record was updated?
5. **CODEOWNERS confidence.** 1000000, or lower (a candidate unless
   corroborated)? CODEOWNERS says who reviews, not always who owns.
6. **Unobserved owners.** A CODEOWNERS team not yet synced is a
   `candidate`, not an owner, until observed. Acceptable?
7. **`owned_by` conflict policy** `set`: too strict? Ship a default
   precedence?
8. **Backfill `valid_from`** where the source knows it (repo creation,
   membership start), so history doesn't start at install?
9. **Record-time compaction** of superseded support versions after a
   window?
10. **Placeholders** from typos in CODEOWNERS: hide by default, or report
    as data-quality issues?
11. **Key types.** Spec-registered per issuer type, with unregistered types
    defaulting to `id`. Or should adapters declare them in `Describe`?
12. **Wire choices.** Enumerations as validated strings rather than proto
    enums, and confidence as integer ppm in the API: confirm.

## Follow-ups

To change once this is approved, in the M1 implementation:

- `schema/observation.v1.schema.json`: new fields, `to` no longer
  required (`from` alternative), `additionalProperties`, attribute `null`.
- `pkg/model`: `Validate` requires `To` today; add the missing fields;
  rewrite the key comment (`model.go:91-94`); `NewObservation` uses
  microsecond times in `id`.
- `testdata/observations.ndjson`: the `pagerduty:`/`aws:` references and
  `on_call_now`.
- `adapters/github`: node IDs as keys with the `X-Github-Next-Global-ID: 1`
  header, names as aliases, `full_name`; send `null`/`[]` for empty
  `language`, `description` and `topics` (`github.go:196-204` omits them);
  snapshot scopes and `complete_sync` for full syncs; `observed_at` per the
  rules above.
- `docs/spec/adapter-protocol.md` and `pkg/adapter`: `emits`, example keys,
  `complete_sync` on the last page, the full-sync claim, the source
  `namespace`, `issues` and `links` fields.
- `docs/spec/contracts.md` and `pkg/contracts`: entities become subjects;
  `GraphStore` gains resolution, merge, un-merge, `Apply`, `as_of`,
  `changes` and conflicts; vector points re-point on merge; `EventBus`
  superseded by ADR 7's `EventLog`, with a home for the new event types;
  conformance tests for every rule here, including apply-order independence.
- ADR 6 says `v1` packages, issue #11 says `v1alpha1`: reconcile.
- ADR 7: partition key (`(source, key)` vs subject), event IDs that
  include the source, and the new event types (manual operations,
  `ValidTimeBoundaryReached`, derived deletions); ADR 2: subject/fact
  terminology and open question 1.
- ADR 10: `namespace`, `issues`, `links` and `confidence_groups` as
  `Source.spec` and configuration fields; adapters declare their issuer
  type in `Describe` for the `namespace` default.
- `AGENTS.md`, `README.md` and the `new-adapter` skill: keys, aliases,
  snapshots and the "send `null` for empty" rule in the adapter checklist.
