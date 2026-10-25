# Concepts

How Bearing turns what your tools say into answers you can check. The
decisions behind this page are [ADR 2](../adr/0002-facts-carry-provenance.md),
[ADR 6](../adr/0006-small-data-model.md) and
[ADR 7](../adr/0007-records-identity-links-matchers.md); the fields are defined in
`proto/bearing/model/v1alpha1/observation.proto`.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../diagrams/concepts-dark.svg">
  <img src="../diagrams/concepts.svg" alt="How data moves through the store. An adapter, reading GitHub, a directory or Kubernetes, sends observations to Store.Ingest, which skips IDs it already has and writes four kinds of table in the DuckDB store: observation, fact with fact_source, records (entity with alias) and identity links (identity_link with link_deposit). Each fact points back at the observations behind it, its provenance. Asserted same_as links join records into one entity. Queries from the CLI, API and MCP read facts (Facts, Walk, Changes) and entities (Entity, Find).">
</picture>

## Observation

An **observation** is one adapter's report about one thing at one moment:
"repository `acme/payments-api` exists, its language is Go, and the
`payments` team approves its changes". It has an ID, the time the source was
read, the thing's **key**, and what was seen about it.

Bearing keeps every observation exactly as it arrived. An observation with
an ID it already has is skipped, so sending the same data twice is safe.

## Key

A **key** names a thing in one system: `github:repo/acme/payments-api`,
`authentik:user/7f3c…`, `email:person/jane@acme.example`. The shape is
`<system>:<type>/<id>`. A key never changes meaning: if a repository is
renamed, the adapter keeps its stable ID in the key and the new name in an
attribute.

## Entity

A **record** is what one source says about one key, with the facts it
observed. An **entity** is one real thing (a service, a repository, a
person, a team): a cluster of records joined by asserted `same_as` links
([ADR 7](../adr/0007-records-identity-links-matchers.md)). Reads answer per
entity, so asking about any of its records shows all of its facts.

Records are never merged. When an observation names a key that another
record already holds, Bearing opens a `same_as` link between the two and
keeps both:

- **Asserted** when the shared key is the other record's primary key (some
  observation is about that key), or when the other record has no kind yet
  (a relation target nobody has described). The records join one entity.
- **Proposed** when the key is only an alias of the other record. A
  proposal carries its evidence and never answers ownership or dependency
  questions.

A `distinct_from` link keeps a pair apart for good: no `same_as` link is
opened for it. Closing a link splits the entity again, and nothing is lost.
Records of different kinds never join.

**Who can assert a link.** A source whose declaration vouches for a key, a
rule the settings name, or a person. A matcher only proposes. A person
decides through the API or `bearing link`, always as a decision that is
logged and applied in order ([API](api.md), "Link decisions"): accept a
proposal (it becomes asserted), reject one (it closes and a `distinct_from`
keeps the pair from being proposed again), join two records, split two joined
records (a `distinct_from` again), or undo a `distinct_from`. Each decision
leaves a deposit on the link with who decided, when and why, so the evidence
shows it. Only a person decides: a caller known to be an agent is refused.

## Fact

A **fact** is one statement about an entity over a span of time:

| Part | Example |
| --- | --- |
| Subject | the `payments-api` repository |
| Predicate | `approves_changes` |
| Object or value | the `payments` team (a relation), or `"Go"` (an attribute) |
| Qualifiers | how a relation holds, for example `protocol: http`, `port: 8080`, `via: env:LEDGER_URL` |
| Valid from, valid to | when a source first saw it, and when it saw it end (empty while it holds) |
| Status | `asserted` (from a source's own records) or `proposed` (inferred, never used to answer ownership or dependency questions) |
| Confidence | 0 to 1 |
| Sources | the observations behind it |

A fact **opens** the first time a source reports it, is **refreshed** each
time the source reports it again, and **closes** when the source says it
is gone: the relation is marked absent, a full listing no longer includes
it, the attribute has a new value, or the thing was deleted.

### The runtime attribute

A `Repository` or `Component` carries `runtime`, the language versions its
manifests declare. Its value is a list of `{language, version, file}`:
`language` is lower case (`go`, `node`, `python`; names from `.tool-versions`
are kept as given), `version` is the text the manifest declares (`1.22`,
`>=18`) and `file` is the manifest. The list in `entity.attributes` is the
complete set from that source: a declaration missing from it closes, and an
empty list closes them all. Each declaration is its own fact, so a bump in
one file does not touch the others.

## Provenance

Every fact keeps the observations behind it. An answer such as "the
payments team owns payments-api" comes with "because GitHub's CODEOWNERS
said so, seen at 06:00, here is the link". When two sources say the same
thing, the answer shows both.

## Time

There is one time line: when a source observed something. Asking "who
owned this last Tuesday?" reads the facts that held then; "what changed
this week?" lists the facts that opened or closed in it.
