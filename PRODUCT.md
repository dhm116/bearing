# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

The design surfaces are the project website (`site/`, a Go static generator
published to GitHub Pages), the README and docs on GitHub, and the plain-text
output of the `bearing` CLI. There is no web app UI.

## Users

Primary: **evaluators**. Platform leads and engineers deciding whether to
adopt, run or contribute to Bearing. They arrive at the project site or the
GitHub README, often new to Bearing and sometimes new to software engineering
(the site's writing rules assume this), and need to understand the everyday
problem, see it answered, and judge whether the answers can be trusted.

Secondary: **engineers asking questions** mid-task (who owns this, what
changed, what depends on it) through the CLI, the REST API or an AI agent
over MCP.

## Product Purpose

Bearing answers questions about an engineering organization from the tools
it already uses. Small adapters read a source (GitHub, an identity provider,
Kubernetes) and emit observations; the core joins records that describe one
thing, stores what was said as facts with their provenance, and answers
queries.

The MVP is done when it answers these five, with the evidence behind each,
on the fictional Acme org, running in Docker Compose:

1. Which team owns service X?
2. Which services use language X below version Y?
3. What changed between times X and Y?
4. What does service X depend on, and how?
5. What depends on service X, and how?

## Positioning

**Live, sourced answers.** Every answer is built from tools the organization
already uses, kept current on its own, and shows where it came from: the
file, line, observation and confidence behind each fact. Bearing integrates
rather than replaces (Backstage included); it defines the data worth having
and keeps adapters small.

## Operating Context

- Evaluators read the project site, the README and `docs/demo.md`, and run
  the demo: `docker compose up` starts Bearing against the Acme fakes.
- Engineers use `bin/bearing` (`owner`, `get`, `related`, `changes`, `find`,
  `runtimes`, `links`), the REST API, or `bearing mcp` from Claude Code or
  Claude Desktop.
- Operators run `bearing server --settings` with one settings file and one
  block per adapter; a person reviews proposed identity links with
  `bearing links --proposed` and `bearing link accept|reject|join|split`.

## Capabilities and Constraints

Describes the `pivot` branch (restarted 2026-10-20 from a smaller scope;
everything earlier is tagged `pre-pivot`).

- Adapters: GitHub (repositories, teams, members, CODEOWNERS, language
  versions from manifests), Kubernetes (workloads, namespaces, team labels,
  source repositories, dependencies between workloads), Authentik directory
  (users, groups, memberships, GitHub account links). AWS and PagerDuty are
  planned.
- Adapters run over gRPC: local ones on a private socket, remote ones over
  mutual TLS. They are stateless and read-only against their source.
- Store: DuckDB inside the server process (`duckdb:///path` or `mem://`),
  one writer. Facts carry provenance.
- Identity: records joined by identity links, asserted when a source or rule
  vouches for them, proposed with decaying evidence otherwise; a person
  reviews proposals. Precision and recall are scored on a labelled Acme set.
- API: one static token. The audit log, OIDC, PostgreSQL and the vector
  index were removed in the pivot.
- Undecided / later: search (full-text, vector), inference of facts from
  prose, attestation of provenance, automation, scale beyond one writer,
  Kubernetes and remote deployment.
- Terminology: observation, entity, fact, record, identity link (asserted or
  proposed), adapter, source.
- Status is an early restart; spec and protocol are drafts and change.

## Brand Commitments

From `docs/brand/README.md`, binding:

- Name: **Bearing** in running text, `bearing` for commands; wordmark all
  lowercase.
- Mark: a lowercase b whose stem ends in a north arrow and whose bowl is a
  compass ring with a center pin. Use the SVGs in `docs/brand/`; never
  redraw, recolor the ink, rotate, outline or shadow it; not below 16 px.
- Accent: deep sea teal (`#1D5E74` light, `#7FC0D6` dark); neutrals lean
  toward it.
- Type: Bricolage Grotesque (display, wordmark), IBM Plex Sans (text),
  IBM Plex Mono (code).
- Voice (site): lead with the everyday problem, present tense describing
  the finished product, plain terms, prefer a diagram to a paragraph, no
  milestones or ADR citations on hand-written pages. The roadmap page alone
  says what is available, in progress or planned.

## Evidence on Hand

- The Acme scenario: fakes in `internal/fakes`, recorded feeds in
  `testdata/acme/`, and one golden answer per MVP question in
  `internal/e2e/testdata/questions/`.
- `docs/demo.md`: real CLI output for each of the five questions.
- `docs/identity-evaluation.md`: the matcher's measured precision and recall.
- The home page's "one question, two ways" story (`site/templates/home.html`).
- No real customers, users, testimonials, benchmarks or press exist. Acme is
  fictional and must always be presented as such. Do not invent adoption
  claims.

## Product Principles

1. **Show the evidence.** An answer without its source, line and confidence
   is incomplete; surfaces should make provenance visible, not hide it.
2. **Integrate, don't replace.** Bearing sits beside the tools a team already
   has; never position it as a replacement for them.
3. **Plain terms first.** Lead with the everyday question, then the answer;
   technical depth is one click away, not in the way.
4. **Honest status.** Describe the product in the present tense, and let the
   roadmap alone say what works today. Never overstate what exists.
5. **Refuse rather than guess.** When something can't be read, Bearing lists
   it apart; surfaces should show unknowns rather than paper over them.
