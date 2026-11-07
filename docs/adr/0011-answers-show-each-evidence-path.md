# 11. Answers show each evidence path

Date: 2026-11-06 · Status: proposed

## Context

[ADR 7](0007-records-identity-links-matchers.md) keeps each source's
record apart and joins them with `same_as` links into clusters, and says
reads answer per cluster. Answers do that today: an owner is the cluster,
named after its oldest record with a kind and a name. Which record is
oldest depends on which source's page was stored first, so "Payments"
(GitHub) and "payments" (the directory) swap from run to run, and the
name says nothing about where the answer came from.

Picking one name hides the evidence. In this example, "Which team owns the
frontend application?" matches two records:

- **GitHub:** the `frontend-spa` repository's CODEOWNERS name the team
  "design".
- **PagerDuty:** the `frontend` service's escalation policy names
  "UI Team".
- **The link:** the matcher proposes that "design" and "UI Team" are one
  team, because they share most of their members.

The question settles neither kind, so a good answer gives both claims and
says how they relate.

| Option | Pros | Cons |
| --- | --- | --- |
| A cluster name, chosen by a rule (oldest, source precedence) | One name per answer | Hides where each claim came from; a rule picks a winner the evidence doesn't |
| One claim per evidence path, with the identity links between them | Each claim names its own record and source; ties need no winner | Answers are longer; the query layer walks record-level paths |

## Decision

- **Subject.** A question starts from every record it matches: the record
  a key names, or every record a name lookup finds, across the kinds the
  question allows and their descendants (ADR 10).
- **Claims.** An answer is a list of claims, one per evidence path. A path
  runs from a starting record to a record of the answer's kind:
  - It goes through asserted facts, and through asserted `same_as` hops
    when a joined record holds the fact.
  - Each claim names the records at its ends by their own names, not the
    cluster's, and carries the facts and sources behind it.
  - Each claim also keeps both clusters' IDs, so a reader can still group
    claims by cluster.
- **Shortest paths.** A claim's path is the shortest one that reaches that
  fact. A path with fewer `same_as` hops comes first.
- **Relations.** For every two records at the end of claims, the answer
  says how they relate, with the evidence:
  - an asserted `same_as`: the same thing, and which rule or decision says
    so;
  - a proposed `same_as`: likely the same thing, with its score and its
    features (for example the share of members in common);
  - an asserted `distinct_from`: not the same thing;
  - no link: nothing.
- **No winner.** Claims are never merged or chosen between. They are sorted
  by source, then key, only so that the output is stable.
- **ADR 2 holds.** Claims read only asserted facts. A proposed link never
  adds a claim. It only describes how two claims relate.
- **Rendering.** The API and MCP return claims and relations as data. The
  CLI renders them as sentences, such as "GitHub says design owns the
  frontend-spa repository. PagerDuty says UI Team owns the frontend service.
  The two teams share 92% of their members and are likely the same team."
- **Clusters stay.** `Get`, `Find` and `Links` still answer per cluster,
  and clusters still group records. This ADR supersedes ADR 7's "reads
  answer per cluster" for answers to questions only.

## Consequences

- An answer's names come from its evidence, so they no longer depend on
  ingest order. The flaky demo test asserts each claim exactly again.
- `docs/edge-cases.md`'s "different names for one entity" entry closes;
  source precedence for names isn't needed.
- The answer types for the five MVP questions change (`query.Ownership`
  and the runtime, change and dependency answers), and so do their goldens,
  the CLI output and the MCP tools. `Ref` names a record and carries its
  cluster.
- The query layer walks paths over records, not clusters. Bounding the
  path length keeps the walk small.
- Answers grow when many sources agree. The CLI can fold claims whose
  records share a cluster into one line. The API keeps every claim.
- ADR 7 is superseded in part when this is accepted.
