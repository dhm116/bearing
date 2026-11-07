# 11. Answers show each evidence path

Date: 2026-11-06 · Status: proposed

## Context

[ADR 7](0007-records-identity-links-matchers.md) keeps each source's
record apart and joins records with `same_as` links into clusters, and
says reads answer per cluster. Answers do that today: an owner is a
cluster, named after its oldest record with a kind and a name. Which
record is oldest depends on which source's page was stored first. So
"Payments" (GitHub) and "payments" (the directory) swap from run to run
(the demo test's stopgap in dhm116/bearing#313), and the name says
nothing about where the answer came from.

Picking one name hides the evidence. "Which team owns the frontend
application?" matches two records:

- **GitHub:** the `frontend-spa` repository's CODEOWNERS name the team
  "design".
- **PagerDuty:** the `frontend` service's escalation policy names "UI
  Team".
- **The link:** the matcher proposes that "design" and "UI Team" are one
  team, because they share most of their members.

The question settles neither kind. A good answer gives both claims and
says how they relate.

| Option | Pros | Cons |
| --- | --- | --- |
| A cluster name, chosen by a rule (oldest, source precedence) | One name per answer | Hides where each claim came from; a rule picks a winner the evidence doesn't |
| One claim per asserted fact, with how the claims' records relate | Each claim names its own records and source; nothing needs a winner | Longer answers; the query layer works on records |

## Decision

- **Subject.** A question starts from the records it matches: the one a
  key or ID names, or every record a name lookup finds of the kinds the
  question allows (descendants included, ADR 10). Each claim carries how
  its subject matched: by key, by exact name, or fuzzily with a score. So
  a fuzzy match is shown as evidence, never decided silently (ADR 2).
- **Claims.** A claim is one asserted fact, with its sources. Its path
  runs from a subject record to the fact's subject record, then along the
  fact to its object, and stops there.
  - A `same_as` hop moves between records of one cluster, as clusters
    stand at the answer's time. It is one hop however long the chain of
    links behind it. Link rows the store refused to cluster (ADR 7, ADR
    10) are never followed.
  - A path takes at most one `same_as` hop before each fact, and as many
    facts as the question walks (dependencies use `Walk`'s depth).
  - A fact reached by several paths is one claim, with its shortest path:
    fewer facts, then fewer `same_as` hops.
  - The object counts when its kind, retired or not, or its predicate's
    range for a kindless record, is the answer's kind or a descendant.
  - Each record in a claim is named by its own name and keys and carries
    its cluster's ID. Each `same_as` hop carries the links that join the
    cluster.
- **Relations.** For every two claims whose objects are in different
  clusters, the answer says how those clusters relate. The first rule that
  applies wins:
  1. An asserted `distinct_from` between any of their records: not the
     same thing. Any proposal between them is listed as contrary evidence.
  2. Open proposed links (`same_as` or `distinct_from`) between their
     records: likely the same, or likely not. They are listed highest
     score first, with the score decayed to the answer's time and the
     link's features (for example the share of members in common).
  3. Otherwise: no relation, and the pair is left out.
  
  Objects in one cluster need no relation, because their hops already show
  the asserted links that join them.
- **Order.** Claims are never merged or chosen between. They are sorted
  by:
  1. path length;
  2. the claim's first source name;
  3. the object record's first key;
  4. the subject record's first key;
  5. fact ID.
- **The five questions.**
  - **Q1 (owners):** starts at the service's records.
  - **Q2 (runtimes):** one claim per runtime fact, starting at each
    matching record.
  - **Q3 (changes):** lists facts per record. It also lists the identity
    links opened and closed in the window, since a join or split changes
    answers without any fact changing.
  - **Q4 and Q5 (dependencies):** walk facts from, or to, the service's
    records.
- **ADR 2 holds.** Claims read only asserted facts. A proposed link never
  adds a claim; it only describes how two claims relate.
- **Rendering.** The API and MCP return claims and relations as data;
  agents write their own text. The CLI generates its sentences from that
  data in one place, and words a proposal as "likely", never as a fact.
  For example: "GitHub says design owns the frontend-spa repository.
  PagerDuty says UI Team owns the frontend service. The two teams share 92%
  of their members and are likely the same team."
- **Clusters stay.** `Get`, `Find` and `Links` answer per cluster. This
  ADR supersedes ADR 7's "Reads (`Entity`, `Facts`, `Walk`) answer per
  cluster" for answers to the five questions. Those answers read records
  through a new record-level read; `Entity`, `Facts` and `Walk` keep their
  cluster meaning.

## Consequences

- An answer's names come from its evidence, so they no longer depend on
  ingest order. The demo test then asserts each claim exactly.
- Every surface that shows an answer changes:
  - the answer types in `pkg/query` (`Ref` names a record and carries its
    cluster; owners, runtimes, changes and dependencies return claims and
    relations; the team-members block follows);
  - their goldens;
  - the REST shapes in `docs/spec/api.md`;
  - the CLI's output and help;
  - the MCP tools' descriptions and schemas;
  - `docs/spec/concepts.md`'s cluster section.
- The implementing PR deletes `docs/edge-cases.md`'s "different names for
  one entity" entry. It also marks ADR 7 "superseded in part by ADR 11".
- The record-level read must not list every path: it finds reachable
  clusters first, then rebuilds each chosen fact's record path.
- Answers as of a past time use today's clusters until membership is
  computed from the links open at that time (`docs/edge-cases.md`).
