# Data model

Version 0.2 (draft). Needs approval from the data reviewer and the
maintainer before code follows (issue #13, milestone M1).

## Changes in this revision

v0.1 described entities keyed by source keys, relations between them, and
facts with a confidence and "history", without saying how keys become
entities, how history works or what wins when sources disagree. v0.2 makes
those rules exact, so two independent implementations given the same events
build the same graph.

| Change | Why |
| --- | --- |
| A **subject** is the stable thing everything attaches to. Its ID is a Bearing-minted UUIDv7 that never changes and is never reused. | Source keys change (repo renames, transfers, login changes); answers, links and audit records need an anchor that doesn't. |
| Source keys become **aliases** of a subject, classed `id` (immutable) or `name` (can be renamed or reused). Name bindings are time-bounded. Replaces v0.1 [Keys](#identifiers-and-aliases). | A rename or transfer keeps the subject. A reused name gets a new subject. |
| Exact **resolution, minting, merge and un-merge** rules, all audited. | Identity is the part of the model most likely to diverge between implementations. |
| **Facts** are `(subject, predicate, object)` where the object is a subject or a typed value. Attributes are facts too. Relation types become entries in a **predicate registry**. Replaces v0.1 [Facts](#facts-and-supports). | One model for metadata and relations; everything gets provenance, time and confidence. |
| Every fact has **valid time** (`valid_from`, `valid_to`) and every support has **record time** (`recorded_at`, `retracted_at`). Defines "as of" and "what changed". | Time-bounded facts (memberships with end dates, ownership history, on-call) are part of the core model, not special cases. On-call becomes one optional enrichment. |
| **Supports**: one per source per fact, ordered by latest `observed_at`. A retraction ends one source's support, not the fact. | A stale full sync can't undo a newer webhook; re-applying an event is a no-op. |
| **Snapshot scopes**: an observation can declare it is complete for `(source, subject, predicates)`. | The only safe way to learn that something stopped being true (a team removed from CODEOWNERS). |
| **Confidence** combines by max within a source system and noisy-OR across systems, rounded to 6 places. Status is `asserted`, `conflicted`, `candidate`, `overridden` or `none`. | Corroboration across independent systems should raise confidence; re-reading the same system should not. |
| **Conflicts** are derived, first-class and surfaced. Ownership conflicts block assertion until resolved. | ADR 2: a fuzzy or disputed owner must never page a team. |
| **Cross-system identity** rules for people (GitHub and the directory), with strong and weak evidence. Weak evidence can never merge. | The MVP joins GitHub with an identity directory (Authentik). |
| Additive, optional observation fields: `entity.aliases`, `entity.linked_ids`, relation `from`, `valid_from`, `valid_to`, `confidence`, `data.facts`, `data.complete`. The CloudEvents type stays `dev.bearing.observation.v1`. | v0.1 adapters stay valid. The Protobuf definitions (issue #11) encode this shape. |

Not changed here: `pkg/model`, `pkg/contracts`, the JSON Schema,
[contracts](contracts.md) and the [adapter protocol](adapter-protocol.md).
They follow once this is approved (see [Follow-ups](#follow-ups)).

## Terms

| Term | Meaning |
| --- | --- |
| **Subject** | A thing Bearing knows about: a person, team, repository, … Identified by a subject ID. |
| **Kind** | The subject's type (`Person`, `Repository`, …). Fixed when the subject is minted. |
| **Namespace** | The issuer of an external identifier, for example `github` (github.com) or `authentik`. |
| **Alias** | An external identifier `(namespace, key_type, external_id)` bound to a subject. Written as a key, `<namespace>:<key_type>/<external_id>`. |
| **Source** | One configured instance of an adapter ([ADR 10](../adr/0010-configuration-as-resources.md)), for example `github-acme`. The unit of provenance. |
| **Source system** | The namespace a source reads, for example `github`. Sources of one system are not independent of each other. |
| **Observation** | One source's report about one subject at one time (`observed_at`). |
| **Claim** | One statement inside an observation: a fact asserted, ended or withdrawn by that source. |
| **Fact** | `(subject, predicate, object)`. The object is another subject or a typed value. |
| **Support** | One source's backing for one fact: its claims over valid time, with provenance. |
| **Valid time** | When something is true in the world: `valid_from` (inclusive) to `valid_to` (exclusive; `null` = open). |
| **Record time** | When Bearing believed it: `recorded_at` (inclusive) to `retracted_at` (exclusive; `null` = current). |

Intervals are half-open `[from, to)`. Times are RFC 3339 UTC with `Z`.
Implementations MUST store and compare times at microsecond precision,
truncating anything finer.

## Determinism

Two conforming implementations that apply the same events in the same order
MUST produce graphs that are identical up to a one-to-one renaming of
subject IDs and up to the values of `recorded_at`/`retracted_at`. That
covers: the subjects and their kinds and statuses; alias bindings; facts,
supports and their valid-time intervals; confidence and status at every
valid time; and conflicts.

The valid-time state of supports (who claims what was true when) MUST NOT
depend on the order events are applied in
([Ordering](#ordering-and-idempotency)). Identity (minting and merging) does
depend on order, which is why the event log ([ADR 7](../adr/0007-durable-event-log.md))
fixes one, and why a rebuild from the log MUST reuse the subject IDs
recorded when the events were first applied (minting is itself recorded,
see [Audit](#audit)).

## Subjects

```json
{
  "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e",
  "kind": "Repository",
  "status": "active",
  "merged_into": null,
  "minted_at": "2026-09-28T01:30:00Z",
  "minted_by": { "event_id": "evt-01J8…", "rule": "observation" }
}
```

| Field | Rule |
| --- | --- |
| `subject_id` | A UUIDv7 ([RFC 9562](https://www.rfc-editor.org/rfc/rfc9562) §5.7) in canonical lowercase text form (36 characters). Minted by the core only. MUST NOT change and MUST NOT be reused, even after a merge. Consumers MUST treat it as opaque; the embedded timestamp means nothing beyond ordering. |
| `kind` | One of the [subject kinds](#subject-kinds). Set at mint, never changed. |
| `status` | `active`, or `merged` (a tombstone). Subjects are never deleted; a source deleting the thing ends that source's facts about it. |
| `merged_into` | For `merged` subjects, the survivor. Resolving a merged subject follows `merged_into` until it reaches an `active` one. |
| `minted_by.rule` | `observation`, `reference` (a placeholder, see [Minting](#minting)) or `split` (an un-merge). |

**Why UUIDv7:** it is an IETF standard with native types in SurrealDB and
PostgreSQL and in every mainstream language; it is 128 bits like any UUID;
and it sorts by mint time, which gives good index locality and a natural,
deterministic survivor for merges (the older subject). ULID has the same
layout but is not a standard and its base32 text form has no native
database type. Random UUIDv4 would lose the ordering.

A subject that has only ever been referenced (minted by rule `reference`)
and never itself observed is **unconfirmed**: it has no `exists` support.
Queries MAY filter on that.

## Identifiers and aliases

*Replaces v0.1 "Keys". The key syntax is unchanged.*

### Keys

```
<namespace>:<key_type>/<external_id>
github:repo_node/R_kgDOH1a2b3
github:repo/acme/payments-api
authentik:user/7f3c2a9e-1b4d-4c8e-9a0f-2d6e8b1c5a74
```

- `namespace` and `key_type` are lowercase letters, digits and hyphens
  (`key_type` also allows underscores). `external_id` is whatever the
  issuer uses and MAY contain `/`.
- The tuple `(namespace, key_type, external_id)` is what issue #13 calls
  `(source, kind, external_id)`. It is renamed here because "source" means
  a configured source and "kind" means a subject kind.
- `namespace` names the **issuer**, not the adapter instance. Two sources
  reading github.com both use `github`. A GitHub Enterprise Server or a
  second directory gets its own namespace, set in its source configuration
  (for example `ghes-acme`).

### Key types

Every key type is registered with:

| Property | Values | Meaning |
| --- | --- | --- |
| `kind` | a subject kind | Subjects bound to this key type have this kind. Used to mint placeholders. |
| `class` | `id` or `name` | `id`: assigned once by the issuer, never changed or reassigned. `name`: human-meaningful, can be renamed, and can later be reused for a different thing. |
| `per_subject` | `one` or `many` | `name` class only: how many live names of this type one subject holds. Binding a new `one` name closes the subject's previous one. |
| `redirects` | `true` or `false` | `name` class only: whether a stale (closed) name still refers to its last subject, as GitHub does for renamed repositories. |
| `case` | `sensitive` or `insensitive` | `insensitive` external IDs are compared and stored after Unicode simple case folding. |

Registry for the MVP sources (more key types are added by spec change until
adapters can declare them, see [Follow-ups](#follow-ups)):

| Key type | Kind | Class | Per subject | Redirects | Case | Example |
| --- | --- | --- | --- | --- | --- | --- |
| `github:repo_node` | Repository | id | | | sensitive | `github:repo_node/R_kgDOH1a2b3` |
| `github:repo` | Repository | name | one | true | insensitive | `github:repo/acme/payments-api` |
| `github:team_node` | Team | id | | | sensitive | `github:team_node/T_kwDOAB12cd` |
| `github:team` | Team | name | one | false | insensitive | `github:team/acme/payments` |
| `github:user_node` | Person | id | | | sensitive | `github:user_node/U_kgDOA1b2c3` |
| `github:user` | Person | name | one | false | insensitive | `github:user/jdoe` |
| `authentik:user` | Person | id | | | sensitive | `authentik:user/<primary key>` |
| `authentik:username` | Person | name | one | false | insensitive | `authentik:username/jdoe` |
| `authentik:group` | Team | id | | | sensitive | `authentik:group/<primary key>` |
| `authentik:group_name` | Team | name | one | false | sensitive | `authentik:group_name/payments` |
| `<saml-ns>:name_id` | Person | id | | | sensitive | `authentik-saml:name_id/jdoe@acme.com` |

GitHub keys use GraphQL global node IDs for the `id` class: they survive
renames, transfers between owners and login changes. Directory keys use the
directory's immutable primary key. The SAML namespace is the identity
provider that issues the NameIDs, named in configuration.

### Bindings

An alias is **bound** to one subject over a valid-time interval:

```json
{ "key": "github:repo/acme/payments-api", "class": "name",
  "subject_id": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e",
  "bound_from": "2026-09-28T01:30:00Z", "bound_to": "2026-10-01T12:00:00Z",
  "recorded_at": "2026-09-28T01:30:02Z", "retracted_at": null,
  "event_id": "evt-01J8…" }
```

- An `id` alias is bound from its first sighting with `bound_to: null`, and
  stays bound to that subject (or its survivor after a merge) for ever,
  unless an [un-merge](#un-merge) moves it.
- A `name` alias is bound to at most one subject at any valid time. Binding
  it to a different subject closes the old binding at the new binding's
  `bound_from`.
- Binding changes are ordered by `observed_at` like claims
  ([Ordering](#ordering-and-idempotency)): an older observation can't
  re-point a name a newer one moved.
- `bound_from` is the `observed_at` of the observation that bound it;
  `bound_to` is the `observed_at` of the one that moved or released it.

### Resolution

To resolve an observed entity, the core takes its `key`, its `aliases` and
the observation's `observed_at` (`t`), and applies the first matching rule:

1. **Id match.** Look up every `id`-class alias. If they resolve to one
   active subject, use it. If they resolve to two or more, those subjects
   are the same thing (one source reported both IDs for one entity): merge
   them with rule `co_reported_ids`, confidence 1.0 ([Merge](#merge)), and
   use the survivor. If the kinds differ, reject the observation with
   `kind_mismatch` and record an identity conflict.
2. **Name match.** Otherwise look up every `name`-class alias bound at `t`.
   For a subject `S` found this way:
   - If the observation carries an `id` alias of a key type that `S`
     already holds with a **different** value, the name has been reused:
     close `S`'s binding of that name at `t` and go to rule 3.
   - Otherwise use `S` (it **adopts** the new `id` aliases). If names
     resolve to two different subjects, use the one with the lowest
     `subject_id` and record an identity conflict; do not merge.
3. **Mint** a subject with the observation's `kind` (rule `observation`).

Then bind every alias in the observation to the chosen subject at `t`,
closing any other live binding of the same `name` alias and, for
`per_subject: one` key types, the subject's previous name of that type.

A **reference** (a relation's `to` or `from`, or a `linked_id`) carries only
a key. It resolves to the subject the alias is bound to at `t`. If the
alias is a `name` with no live binding at `t`, it resolves to its most
recent binding when the key type `redirects`, otherwise a placeholder is
minted. An unbound key is minted as a placeholder (rule `reference`) of the
key type's kind. A key of an unregistered key type MUST be rejected for that
claim (`unknown_key_type`); the rest of the observation still applies.

Every claim records the alias each of its subject and object resolved
through (`via`), which [un-merge](#un-merge) uses.

### Minting

A subject is minted only:

| Rule | When |
| --- | --- |
| `observation` | An observed entity matches no live alias (resolution rule 3). |
| `reference` | A reference names an alias with no binding (a placeholder). |
| `split` | An un-merge detaches aliases that never had a subject of their own. |

Nothing else mints: not manual facts, not identity rules, not queries.
Each mint is recorded in the audit log with the event that caused it.

### Renames, moves and reuse

| Source event | Effect |
| --- | --- |
| GitHub repository renamed or transferred | Same `repo_node`, new `repo` name: same subject. The old name's binding closes; because `github:repo` redirects, stale references to the old name still resolve to it. |
| GitHub team slug renamed | Same `team_node`: same subject. Stale references to the old slug mint a placeholder (GitHub does not redirect team slugs). |
| GitHub login changed | Same `user_node`: same subject. |
| A name reused by a new repository, user or group | The new entity's `id` alias differs, so resolution rule 2 closes the old binding and mints a new subject. |
| A placeholder later observed | Its name alias resolves; the observation's `id` alias is adopted. |

### Merge

Two active subjects of the same kind merge when:

| Rule | Trigger | Confidence |
| --- | --- | --- |
| `co_reported_ids` | One observation carries `id` aliases bound to different subjects. | 1.0 |
| `identity` | A `same_as` fact between them becomes `asserted` ([Identity](#identity-across-systems)). | as computed |
| `manual` | A person merges them. | 1.0 |

Effects, at record time `r` of the merge:

1. The **survivor** is the subject with the lower `subject_id` (byte order of
   the canonical text form, so the older mint). The other gets
   `status: merged` and `merged_into: <survivor>`.
2. All its alias bindings now count for the survivor. Valid-time intervals
   are unchanged.
3. Every support of a fact with the merged subject as subject or object
   counts, from `r`, for the fact with the survivor substituted. Where that
   is the same fact as one the survivor already had, the supports combine;
   two supports from the same source combine by the
   [ordering](#ordering-and-idempotency) rule. Facts that become
   self-referential (`same_as` from a subject to itself) are dropped.
4. Record-time history before `r` is untouched: "as recorded at" any time
   before `r` shows two subjects.
5. An audit record holds the rule, confidence, the evidence (supports or
   person), both alias sets and the event ID.

Merging subjects of different kinds is never allowed.

### Un-merge

An un-merge takes an active subject `A` and a non-empty, proper subset `D`
of its aliases, and is always explicit (a person, or a withdrawn `same_as`
that caused the merge). At record time `r`:

1. If `D` is exactly the alias set a subject `B` brought into `A` in a
   recorded merge, `B` becomes `active` again (it is the same identity, not
   a reuse). Otherwise a new subject is minted (rule `split`).
2. Bindings of aliases in `D` move to that subject.
3. Supports whose claims resolved their subject or object `via` an alias in
   `D` move with them, from `r`. Supports with no `via` alias (manual and
   core facts) stay with `A` and are listed in the audit record for review.
4. The core records a manual `distinct_from` fact between the two subjects
   (confidence 1.0, override). While it is live, no rule merges them; an
   observation that would (rule 1 of resolution) is rejected with
   `identity_conflict` and surfaced.
5. Audit record as for merge.

## Subject kinds

*Replaces v0.1 "Entity kinds". The kinds are unchanged; they are now the
kinds of subjects.*

| Kind | Meaning | Registered attribute predicates | MVP sources |
| --- | --- | --- | --- |
| `Person` | A human | `name`, `email`, `login` | GitHub, directory |
| `Team` | A group of people | `name`, `slug`, `description` | GitHub teams, directory groups |
| `Repository` | A source repository | `name`, `url`, `default_branch`, `language`, `topics`, `archived` | GitHub |
| `Component` | A service, library, job or website | `type`, `tier`, `lifecycle` | (later) |
| `Package` | A published package | `ecosystem`, `name`, `versions` | (later) |
| `Environment` | Where components run | `name`, `account`, `region` | (later) |
| `CloudResource` | An infrastructure resource | `arn`, `type`, `tags` | (later) |
| `Change` | A pull request, commit, deploy or config change | `kind`, `at`, `url` | GitHub (later) |
| `Incident` | An operational incident | `severity`, `status`, `started_at` | (later) |
| `Schedule` | An on-call rotation | `name` | (later, optional enrichment) |
| `Document` | A runbook, ADR, README or similar | `title`, `url`, `doc_type` | GitHub (later) |

Two v0.1 attributes are dropped: `Change.author` is the `changed_by` fact,
and `Schedule.on_call_now` is a valid-time question. On-call is an enrichment like any other: an on-call source reports
`on_call_for` facts with `valid_from`/`valid_to` per shift, and "who is on
call now" is an [as-of query](#as-of).

## Predicates

*Replaces v0.1 "Relation types". The relation types are kept as relation
predicates.*

Every predicate in the registry has:

| Property | Meaning |
| --- | --- |
| `name` | `[a-z][a-z0-9_]*`. Unregistered attributes get a namespaced name, see below. |
| `domain` | Kinds the subject may have. |
| `range` | Kinds the object may have (a **relation predicate**) or a value type (an **attribute predicate**). |
| `cardinality` | Per source: `one` (a source holds at most one live object; a new one replaces the old) or `many`. |
| `conflict` | Across source systems: `none`, `one` (at most one asserted object) or `set` (systems must agree on the set). See [Conflicts](#conflicts). |
| `threshold` | Confidence at which a fact is asserted. Default 0.9. |

A claim whose subject or object kind is outside the domain or range is
rejected (`domain_mismatch`) and the rest of the observation applies.

### Relation predicates

| Predicate | Domain → range | Cardinality | Conflict |
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

### Attribute predicates

| Predicate | Domain | Value type | Cardinality | Conflict |
| --- | --- | --- | --- | --- |
| `exists` | any | bool | one | none |
| `name` | any | string | one | none |
| `email` | Person | string | many | none |
| `login` | Person | string | one | none |
| `slug`, `description`, `title`, `doc_type`, `language`, `type`, `tier`, `lifecycle`, `ecosystem`, `account`, `region`, `kind`, `severity`, `status`, `url`, `arn` | as in [kinds](#subject-kinds) | string | one | none |
| `at`, `started_at` | Change, Incident | time | one | none |
| `versions` | Package | string | many | none |
| `tags` | CloudResource | json | one | none |
| `default_branch` | Repository | string | one | one |
| `archived` | Repository | bool | one | one |
| `topics` | Repository | string | many | none |

`exists` is implicit: every observation of an entity claims `exists = true`
for it, and `entity.deleted` ends that claim.

An attribute that is not registered becomes the predicate
`<namespace>.<attribute>` (for example `github.visibility`), cardinality
`one` for a scalar and `many` for an array, conflict `none`. Namespacing
keeps two systems' unrelated attributes of the same name apart. Registering
it later is a spec change.

### Core predicates

Only the core emits these. Adapters MUST NOT.

| Predicate | Domain → range | Meaning |
| --- | --- | --- |
| `same_as` | X → X (same kind) | Identity evidence. Asserted `same_as` merges the subjects. |
| `distinct_from` | X → X | Blocks merging; written by un-merge or a person. |

### Value types

| Type | JSON | Canonical form (for equality and fact IDs) |
| --- | --- | --- |
| `string` | string | UTF-8 bytes as given, compared byte for byte. |
| `int` | integer number | Signed 64-bit. |
| `float` | number | IEEE 754 double; `-0` becomes `0`; NaN and infinities are rejected. |
| `bool` | `true`/`false` | |
| `time` | RFC 3339 string | UTC, microseconds. |
| `json` | object | [RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) (JCS) serialization. |

For unregistered predicates the type follows the JSON: string → `string`,
number → `float`, boolean → `bool`, object → `json`; an array is a
`many`-valued predicate with one fact per element. `null` ends the source's
claim for a `one`-cardinality predicate.

## Observations

*Extends v0.1. Every v0.1 observation is still valid and means the same
thing. All new fields are optional.*

```json
{
  "specversion": "1.0",
  "id": "github:repo_node/R_kgDOH1a2b3@2026-09-28T01:30:00Z",
  "type": "dev.bearing.observation.v1",
  "source": "adapter/github",
  "time": "2026-09-28T01:30:00Z",
  "datacontenttype": "application/json",
  "data": {
    "entity": {
      "kind": "Repository",
      "key": "github:repo_node/R_kgDOH1a2b3",
      "aliases": ["github:repo/acme/payments-api"],
      "attributes": { "name": "payments-api", "default_branch": "main", "language": "Go" }
    },
    "relations": [
      { "type": "owned_by", "to": "github:team/acme/payments",
        "attributes": { "pattern": "*", "file": ".github/CODEOWNERS" } }
    ],
    "complete": [ { "direction": "out", "predicates": ["owned_by"] } ],
    "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" }
  }
}
```

| Field | Rule |
| --- | --- |
| `time` | The **`observed_at`** of every claim in the observation. For a sync, the time the adapter read the state from the source; for a webhook, the time the source says the change happened, else the time it was received. MUST NOT be more than 5 minutes after the event's ingest time; the core rejects it otherwise (`observed_at_in_future`), because a future time would win every comparison. |
| `entity.key` | The entity's primary key. SHOULD be an `id`-class key when the source has one. |
| `entity.aliases` | Other keys for the same entity, in a namespace the source issues (its own, or one named in its configuration, such as a SAML NameID namespace). |
| `entity.linked_ids` | Keys in **other** namespaces that the source itself records for this entity, each with the rule that applies: `[{ "key": "github:user_node/U_…", "rule": "directory_link" }]`. Input to [identity](#identity-across-systems); never an alias. |
| `entity.attributes` | Attribute claims (see [Normalization](#normalization)). |
| `entity.deleted` | The source says the entity no longer exists. |
| `relations[].to` / `from` | Exactly one. `to`: the entity is the subject. `from`: the entity is the object (lets a group report its members). |
| `relations[].attributes` | **Qualifiers**: stored on the support, not part of the fact's identity (CODEOWNERS pattern and file, membership role). |
| `relations[].absent` | The source says this relation no longer holds. |
| `relations[].valid_from`, `valid_to`, `confidence` | Optional; see [Claims](#claims). |
| `facts[]` | Attribute claims that need times or confidence: `{ "predicate", "value", "valid_from", "valid_to", "confidence", "absent" }`. |
| `complete[]` | [Snapshot scopes](#snapshot-scopes). |
| `evidence` | Where a person can check the claims. Stored on each support. |
| `id` | Stable for the same entity and time. Used to break ordering ties. |

The provenance `source` of every claim is the configured source whose event
carried the observation, set by the core (ADR 10). The CloudEvents `source`
field is informational and MUST NOT be trusted for provenance.

### Normalization

The core turns each observation into claims about the resolved subject `E`,
all with the observation's `observed_at`:

| Input | Claims |
| --- | --- |
| entity present | `(E, exists, true)` asserted |
| `entity.deleted: true` | Ends every claim of this source where `E` is the subject or object, including `exists`. Releases `E`'s `name` bindings in the source's namespaces at `observed_at`. |
| attribute `k: v` | `(E, k, v)` asserted; arrays give one claim per element |
| attribute `k: null` | Ends this source's claims for `(E, k, *)` |
| relation with `to: T` | `(E, type, T)` asserted, or ended if `absent` |
| relation with `from: F` | `(F, type, E)` asserted, or ended if `absent` |
| `facts[]` item | `(E, predicate, value)` asserted, or ended if `absent` |
| a claim on a `cardinality: one` predicate | Also ends this source's claims for other objects of `(E, predicate)`: an implicit snapshot of that one predicate |

Absence of a fact from an observation is **not** an ending, except inside a
declared snapshot scope or for `one`-cardinality predicates as above.
Adapters see different slices at different times; only they know when a
slice is complete.

### Snapshot scopes

`complete` declares that, for this source, the observation lists **every**
fact it currently claims in a scope:

```json
"complete": [
  { "direction": "out", "predicates": ["owned_by"] },
  { "direction": "in",  "predicates": ["member_of"] }
]
```

| Field | Meaning |
| --- | --- |
| `direction` | `out`: facts with `E` as subject. `in`: facts with `E` as object. |
| `predicates` | Predicate names, or `["*"]` for every predicate in that direction (attributes count only for `out`). |

A scope is `(source, E, direction, predicates)` at `observed_at` `t`. When
the core applies it:

1. Store the observation's [ordering key](#ordering-and-idempotency) as
   the scope's **watermark**. The watermark acts as an **ending** on
   `[t, ∞)`, with that key, for every fact in the scope that the
   observation does not claim, including facts whose claims from this
   source are applied later. So an older claim applied afterwards can still
   say what was true before `t`, but not after it, and the result does not
   depend on apply order.
2. Facts the observation does claim are asserted as usual.

A source MUST declare a scope only when it read the complete set from the
source system. If it could not (permission denied, a file it couldn't
parse), it MUST omit the scope; endings based on a partial read are the
worst kind of wrong.

## Facts and supports

*Replaces v0.1 "Facts".*

### Facts

A fact is identified by `(subject, predicate, object)`:

```json
{
  "fact_id": "5d1f9c…",
  "subject": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e",
  "predicate": "owned_by",
  "object": { "subject": "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f" }
}
```

- `object` is `{ "subject": <subject_id> }` or `{ "value": <v>, "type": <value type> }`.
- `fact_id` is the lowercase hex SHA-256 of the JCS serialization of
  `{ "subject", "predicate", "object" }` with the value in canonical form.
  After a merge, the survivor's facts have new IDs; the old ones stay in
  history.
- A fact has no state of its own. Its confidence, status and valid time at
  any `(valid time, record time)` are computed from its supports.

### Claims

A claim is one source's statement about one fact. It **writes** a state
onto valid time:

| Claim | Writes |
| --- | --- |
| assert, `valid_from` `a` (default `observed_at`), `valid_to` `b` (default `null`), `confidence` `c` (default 1.0) | `asserted(c)` on `[a, b)`; and, if `b` is not null, `ended` on `[b, ∞)` |
| end (`absent`, `null`, `deleted`, snapshot omission, one-cardinality replacement), at `t` = explicit `valid_to` or else `observed_at` | `ended` on `[t, ∞)` |
| withdraw (a source or person says the claim was never true) | `ended` on `(-∞, ∞)` |

An assert with a default `valid_from` means "true at least since
`observed_at`" and writes nothing before it, so re-sending a claim never
moves its start later. `confidence` MUST be in `(0, 1]` and is rounded to
6 decimal places on ingest; to say a fact is false, end it.

### Supports

A source's **support** for a fact at valid time `v`, as recorded at `r`, is
the state written at `v` by the **latest** of that source's claims about
the fact that cover `v` and were recorded at or before `r`, where "latest"
is the [ordering key](#ordering-and-idempotency) and snapshot watermarks
count as endings. If that state is `asserted(c)`, the support is **live**
with confidence `c`; otherwise the source does not support the fact at `v`.

Implementations MAY store claims and evaluate this on read, or materialize
**support versions**, the usual bitemporal rows:

```json
{
  "fact_id": "5d1f9c…",
  "source": "github-acme",
  "adapter": "github@0.2.0",
  "event_id": "evt-01J8…",
  "observation_id": "github:repo_node/R_kgDOH1a2b3@2026-09-28T01:30:00Z",
  "observed_at": "2026-09-28T01:30:00Z",
  "last_confirmed_at": "2026-10-01T06:00:00Z",
  "confidence": 1.0,
  "valid_from": "2026-09-28T01:30:00Z",
  "valid_to": null,
  "recorded_at": "2026-09-28T01:30:02Z",
  "retracted_at": null,
  "reason": "assert",
  "via": { "subject": "github:repo_node/R_kgDOH1a2b3", "object": "github:team/acme/payments" },
  "qualifiers": { "pattern": "*", "file": ".github/CODEOWNERS" },
  "evidence": { "url": "https://github.com/acme/payments-api/blob/main/.github/CODEOWNERS" }
}
```

- One row per maximal valid-time interval on which the source's support is
  live with one confidence and one set of qualifiers.
- `recorded_at` is when the row became part of Bearing's record: the commit
  time of the apply transaction, strictly increasing per store.
  `retracted_at` is when a later apply replaced it. Rows are otherwise
  immutable.
- `reason` says why the row exists: `assert`, `end`, `snapshot`,
  `replaced`, `deleted`, `withdrawn`, `merge` or `unmerge`.
- A claim identical to the live one except for `observed_at` (a re-sync
  confirming what is known) changes no row; it updates `last_confirmed_at`
  and the ordering key only.

Both forms MUST give the same answers to every query in this spec.

### Retraction

Retraction is per source. Ending or withdrawing a claim affects only that
source's support. A fact with other live supports stays (perhaps with lower
confidence). A fact with no live supports at `v` has status `none` at `v`
and is not used in answers, but every earlier support version stays in the
record: nothing is deleted.

An ending sets `valid_to`: "this source says it stopped being true then".
The fact stays true before that in every later "as of" query. A withdrawal
removes the source's claim at all valid times, from that record time on;
"as recorded at" an earlier time still shows it.

## Ordering and idempotency

Every claim, ending and watermark has an **ordering key**:

```
(observed_at, observation_id, content_hash)
```

compared left to right: `observed_at` by time; `observation_id` by UTF-8
byte order; `content_hash` (lowercase hex SHA-256 of the JCS form of the
observation's `data`) by byte order. The greater key is later.

- **Latest wins, per source and fact, per valid time.** At each valid time,
  the claim with the greatest key decides the source's support
  ([Supports](#supports)). An older claim applied after a newer one changes
  nothing where the newer one covers.
- **A stale full sync cannot undo a newer event.** A sync page read at
  10:00 that still lists a team owner has a smaller key than the webhook
  ending it at 10:05, whichever is applied last. The next sync, read after
  10:05, agrees with the webhook.
- **Name bindings** use the same keys: a binding change applies only if its
  key is greater than the key of the last change to that alias.
- **Idempotency.** Re-applying an event already recorded as processed
  ([ADR 7](../adr/0007-durable-event-log.md)) is a no-op. Applying a
  different event carrying an identical observation is also a no-op: every
  claim has a key equal to one already recorded and the same content.
- Different sources never override each other; their supports are combined
  ([Confidence](#confidence)).

The 10:00 page may have been read from a lagging replica of the source.
Bearing cannot detect that; the next sync corrects it.

## Confidence

At valid time `v` and record time `r`, a fact's confidence `C` combines its
live supports:

1. Group live supports by **source system** (the namespace the source reads;
   `manual` and `core/identity` are their own groups). Sources of one system
   read the same data and are not independent.
2. Within a group, take the **maximum**: `g_k = max(c_i)`.
3. Across groups, **noisy-OR**, with groups in ascending group-name order:
   `C = 1 − ∏ₖ (1 − g_k)`.
4. Round `C` to 6 decimal places, half to even.

Max within a system keeps a second GitHub source (or a re-sync) from
inflating confidence. Noisy-OR across systems lets independent
corroboration raise it, which max alone cannot express, and never lowers
it. It assumes the systems are independent; when they are not (one copies
the other), configure them as one group.

| Live supports | Groups | `C` | Status at θ = 0.9 |
| --- | --- | --- | --- |
| github 1.0 | github 1.0 | 1.0 | asserted |
| github-acme 0.8, github-mirror 0.6 (both system `github`) | github 0.8 | 0.8 | candidate |
| github 0.8, catalog 0.7 | 0.8, 0.7 | 1 − 0.2 × 0.3 = 0.94 | asserted |
| extractor 0.6, catalog 0.5 | 0.6, 0.5 | 1 − 0.4 × 0.5 = 0.8 | candidate |

## Status, assertion and conflicts

### Status

A fact's **status** at `(v, r)` is decided in this order:

| Step | Condition | Status |
| --- | --- | --- |
| 1 | No live supports | `none` |
| 2 | A live `manual` override scope covers `(subject, predicate)` | `asserted` (confidence 1.0) if the override asserts this object, else `overridden` |
| 3 | `C` < the predicate's threshold | `candidate` |
| 4 | The fact is in an open [conflict](#conflicts) | `conflicted` |
| 5 | otherwise | `asserted` |

Answers about ownership and policy, and anything else that acts, MUST read
only `asserted` facts ([ADR 2](../adr/0002-graph-is-source-of-truth.md)).
Other statuses are for review and display.

A fact's reported `valid_from`/`valid_to` at `(v, r)` is the maximal
interval containing `v` over which its status, as recorded at `r`, is
unchanged.

### Conflicts

For a `(subject, predicate)` at `(v, r)`, let `A` be the objects whose facts
pass steps 1–3, and for each source system `g` with live supports, let
`S_g` be the objects for which `g`'s group confidence (`g_k` in
[Confidence](#confidence)) is at or above the threshold.

| Predicate `conflict` | There is a conflict when | Conflicted facts |
| --- | --- | --- |
| `none` | never | |
| `one` | `A` has more than one object | all of `A` |
| `set` | two systems with non-empty `S_g` disagree (`S_g ≠ S_h`) | objects in `A` not in every non-empty `S_g` |

Objects every system agrees on stay `asserted`. So if GitHub says
`{payments}` and a catalog says `{payments, platform}`, `payments` is
asserted and `platform` is conflicted.

A conflict is resolved, in this order, by:

1. **A manual override** ([status step 2](#status)): a person states the
   answer for `(subject, predicate)`. It is a snapshot scope from source
   `manual` with `override: true`, so it ends when the person ends it, and
   it is audited.
2. **Configured precedence**: a per-predicate ordered list of source systems
   (for example `owned_by: [catalog, github]`). The first system in the
   list with a non-empty `S_g` decides: its objects are `asserted`, the
   other conflicted objects become `candidate`.

Conflicts are derived, not stored state, but implementations MUST expose
them: a query for open conflicts by subject and predicate, an audit record
when one opens or closes (as of the record time that caused it), and an
event (`ConflictOpened`, `ConflictResolved`) on the event log so they can
be routed to people.

```json
{
  "subject": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e",
  "predicate": "owned_by",
  "valid_from": "2026-10-02T09:00:00Z",
  "positions": [
    { "source_system": "github",  "objects": ["0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f"] },
    { "source_system": "catalog", "objects": ["0192b1c4-6000-7c5e-a04f-8b3c4d5e6f70"] }
  ],
  "resolution": null
}
```

## Identity across systems

Adapters never decide that keys from two systems are the same thing. They
report what their own system records (`aliases` in namespaces they issue,
`linked_ids` for others). The core turns that, plus its own matching, into
`same_as` facts from source `core/identity`, one support per rule, and
merges when `same_as` is asserted.

For people, GitHub and the directory:

| Rule | Evidence | Confidence | Strength |
| --- | --- | --- | --- |
| `saml_name_id` | GitHub reports, as a `linked_id`, the SAML NameID it stores for an SSO-linked account; the directory reports the same NameID as an alias it issues. | 1.0 | strong |
| `directory_link` | The directory record stores the GitHub user's node ID (configured attribute), reported as a `linked_id`. | 1.0 | strong |
| `directory_link_login` | The directory stores a GitHub login; it resolves through the `github:user` name binding at `observed_at`. | 0.95 | strong |
| `manual` | A person links them. | 1.0 | strong |
| `verified_domain_email` | A GitHub email verified on the organization's domain equals the directory's primary email. | 0.8 | weak |
| `commit_email` | A commit author email on a commit by the GitHub user equals the directory email. | 0.6 | weak |
| `display_name` | Equal display names after Unicode NFKC normalization, case folding and whitespace collapsing. | 0.3 | weak |

- `same_as` confidence combines as in [Confidence](#confidence) with each
  rule as its own group, then, **if no strong rule is live, is capped at
  0.85**. Weak evidence, however much of it, never merges; it produces a
  `candidate` `same_as` for a person to confirm.
- A live `distinct_from` between the subjects blocks the merge and turns
  the `same_as` into an identity conflict.
- Every merge and un-merge is audited with the rules and supports behind it.
- Matching runs only within a kind. Teams and groups are not matched
  automatically in v0.2 (see [Open questions](#open-questions)).

## Queries

All query times default to now. Answers MUST be computed with the rules
above; there is no other notion of "current".

### As of

`as_of(filter, valid_at, recorded_at)` returns, for every fact matching the
filter (subject, predicate, object, status), its status, confidence,
valid-time interval at `valid_at` and live supports, **as Bearing recorded
it at `recorded_at`**. Subjects are resolved through merges recorded by
`recorded_at`, and aliases through bindings valid at `valid_at`.

| Question | `valid_at` | `recorded_at` |
| --- | --- | --- |
| Who owns payments-api now? | now | now |
| Who owned it on 1 June, as we know today? | 2026-06-01 | now |
| What did Bearing answer on 1 June? (audit) | 2026-06-01 | 2026-06-01 |
| Who is on call now? | now | now (on `on_call_for`) |

### What changed

`changes(filter, t1, t2, axis)` returns one entry per fact whose status or
confidence differs between two points:

| `axis` | Compares | Answers |
| --- | --- | --- |
| `valid` | `(t1, now)` with `(t2, now)` | What changed in the world between t1 and t2, as known now. |
| `record` | `(t1, t1)` with `(t2, t2)` | What Bearing's answers changed between t1 and t2, including late-arriving and corrected data. |

```json
{ "fact_id": "5d1f9c…", "from": { "status": "asserted", "confidence": 1.0 },
  "to": { "status": "none", "confidence": 0 },
  "supports_changed": ["github-acme"] }
```

With `axis: record`, implementations SHOULD also be able to list the
support versions and identity operations recorded in `[t1, t2)`.

## Audit

The core writes an audit record ([ADR 8](../adr/0008-audit-log.md)) in the
same transaction as each of these, with the event ID and the rule:

- subject minted; alias bound, moved or released;
- merge, un-merge, `distinct_from` written;
- a fact's status changes at the current valid time (old and new status and
  confidence);
- a conflict opens or closes; a manual override starts or ends;
- an observation or claim rejected (`kind_mismatch`, `domain_mismatch`,
  `unknown_key_type`, `identity_conflict`, `observed_at_in_future`).

Facts and supports answer "what was true when"; the audit log answers "who
or what made it so".

## Worked examples

Subject IDs below are illustrative UUIDv7s; hashes and event IDs are
shortened. `recorded_at` is shown only where it matters.

| Name | Subject ID |
| --- | --- |
| repo `payments-api` | `0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e` (R) |
| team `acme/payments` | `0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f` (P) |
| team `acme/platform` | `0192b1c4-6000-7c5e-a04f-8b3c4d5e6f70` (L) |
| directory group `payments` | `0192b2d0-1a00-7d6f-a150-9c4d5e6f7081` (G) |
| person jdoe | `0192b2d0-1a01-7e70-b261-ad5e6f708192` (J) |

### 1. A repository with a CODEOWNERS team owner

The [observation above](#observations), read by source `github-acme` at
2026-09-28T01:30:00Z. Nothing is bound yet.

1. `github:repo_node/R_kgDOH1a2b3` and `github:repo/acme/payments-api` are
   unbound: mint R (`Repository`, rule `observation`), bind both.
2. `github:team/acme/payments` is unbound: mint P (`Team`, rule
   `reference`, unconfirmed), bind it.
3. Claims, all from `github-acme`, `observed_at` 2026-09-28T01:30:00Z:

| Fact | Status | `C` | valid_from |
| --- | --- | --- | --- |
| (R, exists, true) | asserted | 1.0 | 2026-09-28T01:30:00Z |
| (R, name, "payments-api") | asserted | 1.0 | 2026-09-28T01:30:00Z |
| (R, default_branch, "main") | asserted | 1.0 | 2026-09-28T01:30:00Z |
| (R, language, "Go") | asserted | 1.0 | 2026-09-28T01:30:00Z |
| (R, owned_by, P) | asserted | 1.0 | 2026-09-28T01:30:00Z |

4. The scope `(github-acme, R, out, [owned_by])` gets watermark
   2026-09-28T01:30:00Z. Later, the team's own observation (with
   `github:team_node/T_kwDOAB12cd` and alias `github:team/acme/payments`)
   resolves by name to P and P adopts the node ID.

### 2. Repository rename

`acme/payments-api` is renamed `acme/payments` on 2026-10-01 at 12:00. The
webhook gives:

```json
{
  "id": "github:repo_node/R_kgDOH1a2b3@2026-10-01T12:00:00Z",
  "time": "2026-10-01T12:00:00Z",
  "data": {
    "entity": {
      "kind": "Repository",
      "key": "github:repo_node/R_kgDOH1a2b3",
      "aliases": ["github:repo/acme/payments"],
      "attributes": { "name": "payments" }
    }
  }
}
```

- Resolution rule 1: the node ID is bound to R. Same subject.
- `github:repo/acme/payments` binds to R from 12:00. `github:repo` is
  `per_subject: one`, so `github:repo/acme/payments-api` closes:
  `bound_to: 2026-10-01T12:00:00Z`. It `redirects`, so a CODEOWNERS file
  elsewhere still naming `acme/payments-api` resolves to R.
- `name` is `one`-cardinality: `(R, name, "payments-api")` ends at 12:00 and
  `(R, name, "payments")` starts at 12:00.
- `(R, owned_by, P)` is untouched: not in this observation, and no scope
  was declared.

### 3. A team removed from CODEOWNERS (snapshot)

On 2026-10-02 at 09:00 a full sync reads CODEOWNERS, which now lists only
`@acme/platform`:

```json
{
  "id": "github:repo_node/R_kgDOH1a2b3@2026-10-02T09:00:00Z",
  "time": "2026-10-02T09:00:00Z",
  "data": {
    "entity": { "kind": "Repository", "key": "github:repo_node/R_kgDOH1a2b3",
                "aliases": ["github:repo/acme/payments"] },
    "relations": [
      { "type": "owned_by", "to": "github:team/acme/platform",
        "attributes": { "pattern": "*", "file": ".github/CODEOWNERS" } }
    ],
    "complete": [ { "direction": "out", "predicates": ["owned_by"] } ]
  }
}
```

- `github:team/acme/platform` mints L (placeholder). `(R, owned_by, L)` is
  asserted from 09:00.
- The scope ends `github-acme`'s support for `(R, owned_by, P)` at 09:00
  (reason `snapshot`). It has no other supports, so its status is `none`
  from 09:00 and `asserted` on `[2026-09-28T01:30:00Z, 2026-10-02T09:00:00Z)`.

Support versions for `(R, owned_by, P)` after this apply:

| valid_from | valid_to | recorded_at | retracted_at | reason |
| --- | --- | --- | --- | --- |
| 2026-09-28T01:30:00Z | null | 2026-09-28T01:30:02Z | 2026-10-02T09:00:03Z | assert |
| 2026-09-28T01:30:00Z | 2026-10-02T09:00:00Z | 2026-10-02T09:00:03Z | null | snapshot |

If a webhook observed at 08:55 still listing P arrives late, its key is
older than the 09:00 watermark: nothing changes.

### 4. A directory group membership with an end date

The directory (source `authentik-acme`, namespace `authentik`) reports group
`payments` with its members, and says jdoe's membership ends at the end of
October (for example from a contractor end date the directory holds):

```json
{
  "id": "authentik:group/5b0e…@2026-10-02T06:00:00Z",
  "source": "adapter/authentik",
  "time": "2026-10-02T06:00:00Z",
  "data": {
    "entity": { "kind": "Team", "key": "authentik:group/5b0e…",
                "aliases": ["authentik:group_name/payments"],
                "attributes": { "name": "payments" } },
    "relations": [
      { "type": "member_of", "from": "authentik:user/7f3c…",
        "valid_from": "2026-03-01T00:00:00Z", "valid_to": "2026-11-01T00:00:00Z" }
    ],
    "complete": [ { "direction": "in", "predicates": ["member_of"] } ]
  }
}
```

- G is the group's subject; `authentik:user/7f3c…` resolves to J (minted
  from the directory's observation of the user).
- `(J, member_of, G)` is asserted on `[2026-03-01, 2026-11-01)` and the
  claim writes `ended` from 2026-11-01.
- `as_of(valid_at: 2026-10-15)` → asserted. `as_of(valid_at: 2026-11-02)`
  → `none`, with no further event needed.
- If the directory later extends the end date to 2026-12-01, the newer
  claim wins on `[2026-11-01, 2026-12-01)`.

### 5. A conflicting ownership claim

A catalog source (`catalog-acme`, system `catalog`; for example a Backstage
adapter) observes at 2026-10-02T10:00:00Z that R is owned by P, while
GitHub (example 3) says L. Both at confidence 1.0.

- `owned_by` has `conflict: set`. `S_github = {L}`, `S_catalog = {P}`. They
  differ, so a conflict opens for `(R, owned_by)` from 10:00, and both
  `(R, owned_by, L)` and `(R, owned_by, P)` are `conflicted`.
- "Who owns R?" returns no asserted owner and the conflict
  ([shape above](#conflicts)). Nothing is paged on its strength.
- Resolution A: the operator configures `owned_by: [catalog, github]`. The
  catalog decides: P `asserted`, L `candidate`.
- Resolution B: a person sets the owner to L. A `manual` override scope on
  `(R, owned_by)` asserts L: L `asserted`, P `overridden`. Both actions are
  audited, and the conflict closes.

### 6. An "as of" query

After examples 1–3, today (2026-10-02T12:00:00Z):

```json
{ "filter": { "subject": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e", "predicate": "owned_by",
              "status": ["asserted"] },
  "valid_at": "2026-10-01T00:00:00Z", "recorded_at": "2026-10-02T12:00:00Z" }
```

```json
{
  "facts": [
    {
      "fact_id": "5d1f9c…",
      "subject": "0192b1c4-5e10-7a3c-9d2e-6f1a2b3c4d5e",
      "predicate": "owned_by",
      "object": { "subject": "0192b1c4-5e11-7b4d-8e3f-7a2b3c4d5e6f" },
      "status": "asserted",
      "confidence": 1.0,
      "valid_from": "2026-09-28T01:30:00Z",
      "valid_to": "2026-10-02T09:00:00Z",
      "supports": [
        { "source": "github-acme", "confidence": 1.0, "observed_at": "2026-09-28T01:30:00Z",
          "valid_from": "2026-09-28T01:30:00Z", "valid_to": "2026-10-02T09:00:00Z",
          "recorded_at": "2026-10-02T09:00:03Z", "retracted_at": null }
      ]
    }
  ]
}
```

The same query with `valid_at` now returns L. With `valid_at` and
`recorded_at` both 2026-10-01T00:00:00Z it returns P with `valid_to: null`,
because on 1 October Bearing did not yet know the ownership would end.

## Open questions

For the maintainer:

1. **Directory groups as Teams.** v0.2 maps directory groups to `Team`.
   Should GitHub teams and directory groups be linked automatically when
   GitHub team synchronization ties them, or only by hand?
2. **Verified domain email.** Should a GitHub email verified on the
   organization's domain be strong evidence (≥ 0.9, auto-merge) rather than
   weak (0.8)? Many orgs without SAML would otherwise link every person by
   hand.
3. **CODEOWNERS confidence.** CODEOWNERS says who reviews, not always who
   owns. Should the GitHub adapter claim `owned_by` at 1.0, or lower (for
   example 0.8, a candidate unless corroborated)?
4. **Conflict policy for `owned_by`.** `set` blocks any disputed owner until
   resolved. Is that too strict when one system lists a superset (it only
   blocks the extra owners)? Should precedence ship with a default?
5. **Default valid_from.** A first sync claims everything from the time it
   reads it, so history starts at install. Should adapters backfill
   `valid_from` where the source knows it (repository creation, membership
   start)?
6. **Retention of record time.** Facts are kept for ever (ADR 7). Should
   superseded support versions be compactable after a window, keeping only
   valid-time history?
7. **Placeholder clean-up.** Unconfirmed subjects minted from references
   (typos in CODEOWNERS) live for ever. Hide them by default, or flag them
   as data-quality issues?
8. **Adapter-declared key types.** Should adapters declare key types and
   predicates in `Describe` (a protocol change), or keep both registries in
   the spec?

## Follow-ups

Once this is approved, in the M1 implementation (issue #11 and after):

- Protobuf messages for subjects, aliases, claims, facts, supports and the
  observation additions; validation rules (`valid_to` after `valid_from`,
  confidence in `(0, 1]`, key syntax).
- `pkg/contracts`: `GraphStore` gains resolution, merge, un-merge, `Apply`
  of claims (ADR 7), `as_of`, `changes` and conflicts; conformance tests
  for every rule above, including apply-order independence.
- `docs/spec/contracts.md` and, if key types move to adapters,
  `docs/spec/adapter-protocol.md`.
- The GitHub adapter emits node IDs as primary keys with names as aliases,
  and declares snapshot scopes for full syncs.
