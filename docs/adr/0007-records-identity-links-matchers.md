# 7. Records, identity links and matchers

Date: 2026-10-21 · Status: accepted

## Context

One logical thing (a service, a team, a person) is described by records in
several sources: a GitHub repository, a Kubernetes workload, a catalog
entry; a GitHub team, a directory group, a Kubernetes `team` label.
[ADR 6](0006-small-data-model.md) binds keys to one entity and merges two
entities for good when an observation names keys of both. The Acme
end-to-end test (`internal/e2e`) showed what that costs:

- A link the source itself calls weak (Tom Becker's GitHub login in the
  directory) merges two people, and nothing can undo it.
- Records with no shared key (directory groups and GitHub teams; the
  Kubernetes `kubernetes:team/<label>` keys) never join, and there is
  nowhere to record "probably the same".
- Why two records were judged one is not kept.

The adapter declarations already say which linked key types a source vouches
for (`LinkDeclaration.authority`, for example Authentik vouches for
GitHub `user_node` but not for a login) and how a field can be matched
(`MatchMethod`: name, email, members). `Ingest` never sees them.

| Option | Pros | Cons |
| --- | --- | --- |
| Keep merging; pass authority in | Small change | Still irreversible; still nowhere for weak evidence |
| Records and identity links | Reversible, explainable, room for scored proposals | One more table; reads go through clusters |

## Decision

- **Records.** Each source key is a record: today's `entity` row, one per
  key that a source observes. Facts stay on the record that observed them.
- **Identity links.** A table `identity_link` holds `same_as` and
  `distinct_from` links between two records, each with a status
  (`asserted` or `proposed`), a score from 0 to 1, the rule or matcher that
  made it, and its evidence (observation IDs, the features that matched).
- **Clusters.** A logical thing is a cluster: records joined by asserted
  `same_as` links, never across kinds, never across an asserted
  `distinct_from`. Each record carries a `cluster_id`, recomputed for the
  records a changed link touches. Reads (`Entity`, `Facts`, `Walk`) answer
  per cluster. Closing a link splits the cluster; nothing is lost.
- **Authority.** A link from an observation is asserted when the source's
  declaration marks that linked key type authoritative, and proposed
  otherwise. A source's settings may override the declaration (both ways)
  for one key type. `Ingest` takes each source's declaration.
- **Matchers.** A matcher looks at candidate pairs and proposes links:
  `Match(ctx, candidates) []Proposal`. Candidates come from blocking (same
  kind and a normalized name; shared linked keys). The first matcher is
  transparent: a weighted score over name similarity, shared members or
  neighbours (`MATCH_METHOD_MEMBERS`) and email, with weights in code and
  each feature's contribution kept as evidence. Matchers only ever
  propose.
- **Promotion.** A proposed link becomes asserted only by a person (a
  `same_as` manual event) or a deterministic rule the settings name. No
  matcher promotes its own proposals in the MVP.
- **Evidence over time.** A proposed link's or proposed fact's strength is
  reinforced by each supporting observation and decays with time, as an
  ant trail does: each observation deposits a weight set by its source and
  rule, and the total halves every half-life, set per predicate (days for
  on-call, weeks for a dependency seen at run time). Observations that say
  the link is absent count against it, so the score is Beta-style, with a
  count of support and of contrary evidence, both decayed, giving a
  strength and how sure it is. The score is computed when read, from
  `fact_source` times and the current time, so nothing runs in the
  background and tests drive it with a fake clock. A proposal below a
  floor is closed; one above a threshold is put first for review, never
  promoted. Only independent source observations deposit: nothing Bearing
  inferred or answered can reinforce itself. Asserted facts from
  authoritative listings don't decay; they close when the source says so.
  Decay also lets facts from sources with no complete listing (webhook-only
  or text) fade.
- **Evaluation.** `internal/e2e` gains a labelled identity set from the Acme
  org (Jane and Mei the same, Tom apart, the payments teams joined, the
  Kubernetes `payments` label joined to the GitHub team) and reports
  precision and recall of proposals and recall of blocking. A matcher is
  adopted when it beats the one it replaces there.
  For decay, the set also replays the Acme timeline (the team rename, the
  deleted team, a membership that ends) and reports how long a stale
  proposal outlives its evidence, how often a proposal crosses the floor
  back and forth, and precision over time against the same matcher with
  no decay. Half-lives and weights change only with those numbers.

## Proving it out

1. Records, links, clusters and declaration-driven authority in
   `internal/duckstore`, with the two skipped Acme tests un-skipped.
2. The transparent matcher, its score decayed as above, measured on the
   Acme set with and without decay.
3. A spike branch (`spike/identity-models`) tries a System One decision
   model's same-or-not judgement as one more matcher feature, measured on
   the same set. It calls the System One API (`POST /v1/systemone`), which
   both TypeSafe's hosted [Jev](https://docs.typesafe.ai/introduction) and
   the open [Kev](https://github.com/jaredpalmer/kev) models (Apache-2.0,
   run locally) serve, so one client covers both; Kev is the default, so
   identity data stays in the deployment. The question text and option
   order are fixed and versioned, the probabilities are calibrated on the
   labelled set, and the set is grown from the Acme fakes (renamed teams,
   other naming conventions, near-miss names) until it is large enough to
   calibrate on. Embeddings for blocking are a separate, later experiment.
   The spike merges only through a later ADR.

## Consequences

- A wrong link is a closed link, not a corrupted entity.
- Answers can say why two records are one thing.
- Proposal scores change with time alone; every answer shows its working
  (supporting and contrary observations, the last one's age, the
  half-life).
- Reads join through `cluster_id`; writes recompute clusters for the
  records a link touches.
- Supersedes the merge rule of ADR 6 (`entity.merged_into`) when accepted.
