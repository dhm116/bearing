# 13. Vocabulary as data

Date: 2026-11-12 · Status: accepted 2026-11-26

## Context

Bearing's names for things, what they mean, and the rules that connect
them are mixed together and frozen in place:

- **Names collide.** GitHub's `id` is a number and PagerDuty's is text.
  ADR 10 allows one shape per name, so the generic adapter (ADR 12) can't
  map both.
- **Meanings are frozen.** ADR 10 stores `extends` as a property of a
  kind. A parent can never change, so a kind first configured without one
  can't gain one later.
- **Adapters decide meaning.** The Kubernetes adapter emits every
  namespace as an `Environment`. For most people the cluster is the
  environment.
- **Rules have no provenance.** "The `team` label names the owner" lives
  in adapter code. The fact it makes cites the observation but not the
  rule, so changing the rule changes answers with no record of why.

The three ADR 12 review blockers (adding `extends` later, colliding field
names, and where derive rules live) are all symptoms of this.

| Option | Pros | Cons |
| --- | --- | --- |
| Patch ADR 10 and ADR 12 rule by rule | Small changes | Each new adapter finds the next special case |
| Adopt an OWL ontology with a reasoner and triple store | A standard and its tools | Open-world reasoning can't say "no owner"; `owl:sameAs` merges, undoing ADR 7; a second store beside DuckDB |
| **Treat the vocabulary as versioned data with owners, and mappings and rules as dated records** | Names, meanings and rules each change on their own, with evidence | A new layer to build; ADR 10 is amended |

We borrow ideas from the semantic-web standards (namespaced names, SKOS
`broader` and `exactMatch`, SSSOM's attributed mappings, PROV, SHACL
shapes) without their machinery.

## Decision

- **Every term has a namespaced name.** Namespaces are reverse domains
  (`io.k8s`, `com.github`, `com.pagerduty`). Config may write a short
  name (`k8s:Deployment`) while only one loaded namespace ends in that
  segment; when a second arrives, config using the short name fails to
  load, naming both. Operators may define aliases in settings, and an
  alias wins. Stored names are always full. `core` is reserved for
  `com.dougmccall.bearing.core`.
- **Three owners define terms, and nobody else does:**
  - Bearing maintainers own the **core module** (`vocab/core.yaml`, today's
    kinds and relations), generated into Go and released with Bearing.
  - Each adapter publishes its **adapter module** in `Describe`: terms in
    its own namespace, their shapes, and suggested mappings. A namespace
    belongs to an adapter, not to a source: every source that runs the
    adapter (two clusters, two GitHub orgs) publishes the same module.
    New terms are added, and a source still on an older version publishes
    a subset; a module that changes or narrows a stored term's shape is
    refused, naming the source. A second adapter claiming the namespace is refused unless
    settings pin the namespace to it. Adapters never emit core terms.
  - The operator may publish an **operator module** in the org's own
    namespace (`com.acme`), for terms no tool holds, such as one
    PagerDuty service that stands for several real services.
- **Every module names its source.** The operator module names a repo,
  path and full commit SHA (branches and tags are refused), plus an
  optional `sha256`. Bearing reads a local copy and verifies it against
  the commit when git can reach the repo, or against the checksum when
  offline; boot stops if neither check can run or one fails. Core and
  adapter modules record their build commit. Each term row records where
  it came from and every check that passed (`commit`, `sha256`, or both).
- **Shapes and growth are per term.** A term only grows (ADR 10's rule,
  scoped to the term). A term shares a shape with a core term only
  through `exact_match`. Shapes are checked when a mapping is accepted
  and again at boot: `bearing vocab accept` refuses a mapping that would
  fail, naming what it needs first (such as the `broader` mapping its
  domain depends on), so a mapping decision can't stop Bearing booting.
- **Mappings and rules are records,** with the shape of `identity_link`:
  subject, predicate (`broader`, `exact_match`, `close_match`,
  `derives`), object, status, author, justification, version and
  `valid_from`/`valid_to`. Adapter suggestions and imports start proposed;
  the operator accepts or rejects them. Mappings in the operator's
  settings are asserted when boot first finds them, and ended when boot
  no longer finds them; each is a settings decision, an event like any
  other, and finding an unchanged mapping again is not. A settings
  mapping is changed only by editing the settings: `bearing vocab` refuses
  to decide one. `extends` becomes a dated `broader` row, so it
  can be added later. A term has at most one `broader` term in force; a
  second is refused, until a use case justifies more, and so is a cycle.
  Accepting or ending a `broader` mapping recomputes, in the same
  transaction, the clusters of every record of that term and the terms
  below it, since ADR 7's line-of-descent rule reads it.
- **Answers use today's mappings.** `broader` and `exact_match` are read
  at query time, as clusters are (ADR 11): an answer for a past time uses
  the mappings in force now. A mapping decision goes through the event
  log, as a link decision does, and is an event: Q3 ("what changed")
  lists the mapping decisions in its window, with author and
  justification, beside the facts that opened and closed.
- **Every mapping ends at a core or operator term.** No mapping links two
  adapters' terms, until a use case justifies it.
- **`derives` rules run in the server at ingest.** A rule's key is a Go
  template over the observation's allow-listed fields, with a
  deterministic set of functions under Sprig's names: no environment,
  randomness, clock or key generation. Templates are parsed at boot. One
  that renders empty, or reads a missing field, makes no fact. Rules run
  in the observation's ingest transaction, and a fact a rule makes is
  written as if the observation carried it: the same last-writer-wins on
  `observed_at`, the same guard against a stale page reopening what a
  later one ended, the same snapshot and complete-sync closing. A rule
  keys records in its observation's own system; joining systems is
  identity links' work (ADR 7). Editing a rule is a new version. Boot
  refuses an operator rule whose id and version are stored with a
  different body; an adapter module that does the same with a suggestion
  is refused as a module, naming it, so Bearing still boots.
  `fact_source` gains a `rule` column naming the rule and version, and an
  answer names the versions behind each fact. A rule built from a fixed field asserts; one that guesses
  only proposes (ADR 2). What a template can't express stays in
  adapter middleware (ADR 12), inside the adapter process.
- **A derived fact closes on evidence.** It keeps its observation's
  source. It closes when the rule stops making it, or, when no webhook
  reports the change, when the observation's own adapter reports its
  subject gone on a later sync (a complete snapshot, or an audit log the
  adapter reads). Ending a rule (rejected after it was accepted, ended,
  or deprecated with no successor) closes, in the same transaction, the
  open facts whose only sources are that rule's versions, at the
  decision's time; the decision, with its author and reason, is the
  justification Q3 shows. Other adapters' observations can also support
  closing it, such as a pull request that removes the Deployment; that is
  inferred, so it only proposes the close. A proposed close is a record
  beside the fact, shaped like `identity_link`. Confirming it closes the
  fact at the time the evidence names (the merge), and is refused when
  the fact's own source has observed the subject since.
- **Deprecation, not deletion.** Terms, mappings and rules carry
  `deprecated: {since, replaced_by, consider, reason}`. `replaced_by`
  names exactly one successor of the same shape, checked as an
  `exact_match` is. The deprecation rewrites the old term's open facts to
  the successor, keeping their IDs, start times and sources, and merges
  any into an open successor fact of the same subject, object and source.
  Ingest writes new facts, and snapshot scopes, under the successor, so a deprecation by itself shows
  no change in Q3. A successor of another shape is a `consider`, and the
  change shows. `consider`
  lists candidates, and nothing is rewritten. Chains collapse, cycles are
  refused, and only the owner deprecates. When a successor rule makes the
  same fact, the fact gains a source instead of closing and reopening.
- **Store:** `term`, `prefix` and `mapping` tables replace `kind` and
  `kind_predicate`; `fact_source.rule` is added, and a `fact_close` table
  holds proposed closes. Proto: a `VocabularyModule` in
  `DescribeResponse` and a module version on observations, outside the
  payload and the ID, so deduplication is unchanged. Both are
  additive.
- **Import and export.** Terms never arrive by import. `bearing vocab
  export|import --format yaml` moves mapping packs between instances and
  creates proposed mappings. Later, if asked: Turtle and JSON-LD export
  (SKOS, SHACL, PROV) under `https://bearing.dougmccall.com/ns/`, and an
  SSSOM or Turtle import that creates proposed mappings, or alignment
  labels to outside IRIs that never change a fact.
- **Not doing:** OWL reasoning, a triple store, SPARQL, an external upper
  ontology as the base, or import that defines terms.

## Consequences

- **ADR 10 is amended:** one shape per namespaced term; `extends` is a
  `broader` mapping; names are `namespace:Term` (`io.k8s:Service`, with
  `core` reserved); the operator module, pinned to a commit, replaces the
  `EntityKind` settings resource; and `term`, `prefix` and `mapping`
  replace the `kind` table. **ADR 11 is amended:** answers read today's
  mappings, and Q3 lists mappings accepted, rejected or ended. **ADR 12 gets simpler:** its three blockers are
  answered here, and the generic adapter publishes a module built from
  its mapping.
- The Kubernetes adapter suggests `k8s:Namespace broader
  core:Environment` instead of deciding it. The demo ships suggested
  Acme mappings, so the five questions work out of the box.
- Every answer can say which rule, at which version and by whom, turned
  an observation into the fact.
- **Order of work:** core module and namespaced terms with no change in
  behavior; the new tables; adapter modules in `Describe`; `derives` at
  ingest, plus `bearing vocab review|accept|reject` and YAML packs; then
  ADR 12's generic adapter.
- **Still open:**
  - Copy a deterministic subset of Sprig, or import it (10 modules, about
    10,400 lines, plus a deny-list for the 14 functions "hermetic" still
    lets through). The spike's rules needed 25 functions in 74 lines, and
    the recommendation is to copy them, adding one when a rule needs it.
  - Links that only a trail of clues supports, such as payments-api →
    Datadog monitor → PagerDuty service. Each step is a proposed fact
    from a rule. Joining steps into a proposed relation, reinforced per
    independent claim and checked by a System One model, extends ADR 7's
    matchers and needs its own ADR.
- The spike on [`spike/vocabulary`](https://github.com/dhm116/bearing/blob/ff9c9dd/spike/vocabulary/FINDINGS.md)
  shows this decision working on the Acme fakes.
- The illustrated explainer is
  [`docs/explainers/adr-13-vocabulary.html`](../explainers/adr-13-vocabulary.html).
