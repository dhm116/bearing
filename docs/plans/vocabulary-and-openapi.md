# Implementation plan: ADRs 11, 12, 13 and 14

Status: agreed with ponytail (rounds 1–5), revised for PR #336 review round 1. ADR 14 accepted 2026-11-27. Lead: Opus. Date: 2026-11-27.

This plan builds what the accepted ADRs decide and `main` (`89d0b5d`)
still lacks: ADR 13 (vocabulary as data), ADR 12 (the generic OpenAPI
adapter) with ADR 14's authoring path, and ADR 11's answers above the store. The spike on
`spike/vocabulary` (FINDINGS at `caf5b74`) is the reference. Its code is
**read for ideas, never cherry-picked**: it was built *beside* today's
code in a `namespaced` mode, and the real build replaces the old code in
place (AGENTS.md: delete the rest).

## 1. Invariants every brief repeats

From the spike's PLAN, which the spike's first pass broke:

1. Every term is namespaced: kinds, relations and attributes. Core terms
   keep their bare stored names (`owned_by`, `Component`) as `core`'s one
   fixed alias.
2. Adapters never emit core terms. *This is the end state:* Kubernetes
   keeps emitting today's names until stream K replaces it.
3. Core meaning comes only from mappings (`broader`, `exact_match`) and
   `derives` rules.
4. Every mapping ends at a core or operator term. Adapter suggestions
   start proposed; settings mappings are asserted.
5. `derives` runs at ingest, in the observation's transaction, with the
   same last-writer-wins, stale-page guard and closing as adapter facts.
6. A term only grows; one owner per namespace; `core` is reserved;
   operator modules are pinned to a full SHA and verified.

A shortcut that breaks one is a defect in the stream, never a reason to
change an ADR.

**Two senses of "mapping".** In F2, M, D, O and A, a *mapping* is an ADR
13 record (`broader`, `exact_match`, `derives`). In G, I and K, the
*adapter mapping* is ADR 14's `AdapterMapping` file, which only suggests
ADR 13 mappings.

## 2. Streams

Each stream is one PR. It ends with a green `make check` and an
end-to-end test on the Acme fakes.

| ID | Capability | Needs | Owns |
| --- | --- | --- | --- |
| **F1** | Foundation, names: core module, namespaced names, term and prefix tables, registry, proto | — | `pkg/vocab/{vocab,names,core.yaml}` + generated Go; `internal/duckstore/50_kind.sql` → `50_vocab.sql` (term, prefix), `vocabulary.go`; `pkg/model/{registry,vocabulary}.go`; `proto/…/adapter.proto` (`VocabularyModule`, module version, and whether the adapter takes an adapter mapping) + `gen/` |
| **F2** | Foundation, mappings: mapping table and writer, boot loading of settings mappings and rules, `fact_source.rule`, storing the module version | F1 | `internal/duckstore/mapping.go` (put, version supersession) and its table; `fact_source.rule`; the module version in `ingest.go`; `pkg/server/boot/settings.go` (mappings and rules blocks) |
| **Q** | ADR 11 answers: claims, record refs, match mode, ordering, past-time claims, Q3 joins and splits, CLI sentences, REST and MCP | — | `pkg/query/**`, `internal/duckstore/read.go`, `pkg/graph/graph.go` (record-level reads), `cmd/bearing/{query,mcp}*.go`, `pkg/server/api*` answer handlers, goldens, `docs/spec/api.md`, `docs/mcp.md` |
| **M** | Mapping decisions: accept, reject and end through the event log; cluster recompute on `broader`; `broader` and `exact_match` read at query time; Q3 lists mapping decisions; `bearing vocab review\|accept\|reject` | F2, Q | `internal/duckstore/{mapping_decide,links}.go`, `read.go`, `pkg/graph`, `pkg/query` (after Q), `cmd/bearing/vocab*.go`, `pkg/server/api*vocab*` |
| **D** | Derive rules at ingest, and closing their facts | F2 | `pkg/vocab/{derive,funcs}.go` + `SPRIG-LICENSE`; `internal/duckstore/derive*.go`; the derive hook in `ingest.go`; `CloseRuleFacts(rule, at)`, a store function M's decision path calls |
| **G** | Generic OpenAPI adapter: the `AdapterMapping` proto and its YAML loader with line-numbered errors (ADR 14); engine; per-leaf field allow-list; read-only rule on built requests; credentials bound to the base URL's origin; spec-lock check at start; middleware; module from the adapter mapping; per-kind metrics; a threat-model control | F1 | `proto/bearing/config/v1alpha1/adapter_mapping.proto` + `gen/`; `adapters/openapi/**` (in `spec/`, only `lock.go`); `cmd/bearing-adapter-openapi/`; `docs/security/threat-model.md` (the new control, which cites C-OPS-3) |
| **O** | Operator module, pinned and verified; replaces the `EntityKind` resource; the namespace pin setting; `SourceSpec.adapter_mapping` (resolved, checked and passed to the adapter) | F2 | `pkg/vocab/operator.go`; every `pkg/server/boot/{settings,assemble}.go` change from wave 1 on; `proto/bearing/config/…/resources.proto`; C-OPS-3's wording in `docs/security/threat-model.md` |
| **A** | GitHub and authentik: adapter modules in `Describe`, namespaced emission, namespace ownership check | F2, M, D, O | `adapters/{github,authentik}/**`, `pkg/adapter` `Describe`, the ownership check in `internal/duckstore/vocabulary.go` (F1's file, edited after F1 merges), `demo/settings.json` with the Acme mappings and rules, `testdata/` goldens |
| **I** | The authoring path (ADR 14): `init` writes the adapter directory; `check` offline against the committed, pinned spec; `test` replays fixtures against goldens, with `--explain` | G | `cmd/bearing/adapter_{init,check,test}.go`; `adapters/openapi/spec/` (the spec reader; it extends G's `lock.go` after G merges) |
| **K** | Kubernetes on the generic adapter, then delete the hand-written one | G, I, A, M, D | `adapters/kubernetes/` becomes an ADR 14 directory (`adapter_mapping.yaml`, `spec.lock`, trimmed `spec.json`, `fixtures/` recorded from the Acme fakes, `testdata/`, and `middleware.go` with the env-dependency middleware, embedding the three runtime files by name), replacing the hand-written code there; the e2e comparison; `cmd/bearing-adapter-kubernetes` becomes a small `main` importing that package; `demo/{Dockerfile,settings.json}`, `cmd/bearing-demo-fakes/five_questions_test.go`, `internal/e2e/dependencies_test.go`, `README.md`, `CONTRIBUTING.md`. `testdata/observations/valid/1[0-4]-kubernetes-*.json` stay: they test model validation, not the adapter |

### Why this cut

- **The foundation lands first, in two PRs**, and carries everything two
  streams would otherwise both write. F1 holds names, the term and prefix
  tables, the registry (their caller) and the proto. F2 holds the mapping
  writer and the settings boot path that calls it, `fact_source.rule`
  and the stored module version. Neither is dead code, and each fits one
  review round; one PR would be about 1,500 lines of code and 1,800 of
  tests. No change in behavior: adapters still emit today's names, read
  as core terms.
- **Q runs beside F1 and F2.** It needs only today's store (`graph.Evidence`
  exists, but nothing calls it except `pkg/store/instrument.go`, and
  nothing above the store sets `LinkFilter.ChangedSince`). It touches
  none of F1's or F2's files.
- **M starts after Q merges.** Both edit `read.go`, `pkg/graph` and
  `pkg/query`.
- **D and M split at one call.** D owns making and closing derived facts,
  and exposes `CloseRuleFacts`. M's decide path calls it when a rule is
  rejected, ended or deprecated with no successor. D's close-on-end test
  rebases on M's decide commit.
- **Kubernetes is converted once, in K.** A covers only GitHub and
  authentik. Before K deletes the hand-written Kubernetes adapter, it
  records that adapter's five answers as a golden, and the generic
  adapter must match it. The golden is recorded after A merges, since A
  changes the GitHub and authentik answers it holds.
- **K's fallback, A2.** If the golden comparison shows a gap the
  env-dependency middleware can't close, or K hasn't merged within one
  review round of A, convert the hand-written adapter in place, as the
  spike did (Sonnet; reviewers security, data, go).

## 3. Waves

At most three streams at once.

```
Wave 0   F1 → F2  Q        G (opens when F1 merges)
Wave 1   M        D        O
Wave 2   A        I
Wave 3   K        docs
```

- G opens when F1 merges. D and O open when F2 merges. M opens when F2
  and Q have both merged.
- O owns every `settings.go` change from wave 1 on, so A, which reads
  O's namespace pin, starts after O merges.
- G's first commit is the `AdapterMapping` proto (ADR 14). Until O wires
  `SourceSpec.adapter_mapping`, G's tests pass the directory to the
  adapter directly. I starts when G merges, since `test` replays through
  G's engine.
- The engine enforces read-only on the requests it builds; the read-only
  rule needs no spec at runtime. `check` (I) validates the same rule
  against the spec. G owns only `spec/lock.go`, which it reads at start,
  and I owns the rest of `spec/`, so G and I share no file.

## 4. Team

Models follow AGENTS.md: Opus for rulings and data-correctness traps,
Sonnet for implementation against an approved plan.

Reviewers come from `.github/reviewers.yml`. Security takes a slot
whenever it matches. Ponytail referees every round.

| Stream | Implementer | Model | Why | Reviewers |
| --- | --- | --- | --- | --- |
| F1 | lead (`golang-pro`) | Opus | the shared contract; wrong early calls are costly | data, spec, go |
| F2 | lead (`golang-pro`) | Opus | the mapping writer and boot path every later stream uses | security, data, spec |
| Q | `golang-pro` | Opus | ordering, past-time claims and evidence paths | security, data, spec |
| M | `golang-pro` | Opus | recomputing clusters under a `broader` in one transaction (ADR 7's line of descent) | data, spec, go |
| D | `golang-pro` | Opus | stale-page guard, closing on evidence, supersession | security, data, go |
| G | `golang-pro` | Sonnet | new package against a clear contract | security, go |
| O | `golang-pro` | Sonnet | small, well specified | security, spec, data |
| A | `golang-pro` | Sonnet | per adapter, once M and D exist | security, data, spec |
| I | `cli-developer` | Sonnet | CLI ergonomics and spec guessing | security, spec, go |
| K | `golang-pro` | Sonnet | a mapping, one middleware, a golden | security, go |
| A2 (fallback) | `golang-pro` | Sonnet | converts the hand-written Kubernetes adapter in place | security, data, go |
| docs | `technical-writer` | Sonnet | docs last, junior audience, diagrams with `diagram-design`; includes the adapter authoring guide (ADR 14's loop, end to end) | spec |

## 5. Acceptance

Each is a test that fails if the rule breaks.

- **F1**
  - `make generate` leaves no diff, and the old registry tests pass on
    the generated core.
  - Names resolve short to full, refuse a bare namespace, fail naming
    both when a short name turns ambiguous, let an alias win, and keep
    `core` reserved.
  - A term's shape only grows.
  - Every existing e2e test passes unchanged.
- **F2**
  - One observation sent with two module versions stores one row.
  - Settings mappings and rules load at boot, and a new version of a
    mapping supersedes the old one.
  - Every existing e2e test passes unchanged.
- **Q**
  - Claims and relations come back with the record and its cluster.
  - The match mode and "limit hit" are reported.
  - Ordering rules 2–5 hold.
  - A past answer shows facts closed or opened later, with their recorded
    times.
  - Q3 lists joins and splits with their justification.
  - The CLI says "likely" for a proposed claim, and REST and MCP return
    the same answer as the CLI.
  - Housekeeping: delete the edge case "Two sources name one thing
    differently" and mark ADR 7 "superseded in part by ADR 11".
- **M**
  - Accept, reject and end go through the event log.
  - A `broader` added after the first boot is accepted, and recomputes,
    in the same transaction, the clusters of every record under the term.
  - A second `broader` and a cycle are refused.
  - A mapping between two adapters' terms is refused.
  - `exact_match` is shape-checked at accept and at boot, and accept
    names what it needs first.
  - Answers read today's mappings.
  - Q3 lists mapping decisions with author and justification, and never
    proposed mappings.
  - Settings mappings are ended when boot no longer finds them, and
    `bearing vocab` refuses to decide one.
- **D**
  - Templates parse at boot; a bad one fails, naming the rule. `now`,
    `env` and `randAlpha` fail to parse.
  - A rule reads only allow-listed fields; an empty render or a missing
    field makes no fact.
  - `fact_source.rule` names `id@version`.
  - A new version making the same fact adds a source.
  - **A stale page after a webhook leaves one open owner.**
  - **Ending a rule closes the facts only it backs.**
  - A deleted workload's fact closes on its adapter's next complete sync.
  - A stored operator `id@version` with a different body stops boot; an
    adapter suggestion doing the same is refused as a module while
    Bearing boots.
  - The Sprig subset is copied, with its licence, and its list functions
    accept `[]any`.
  - The Q1 answer names `rule@version` in the CLI, REST and MCP.
- **G**
  - A YAML adapter mapping is walked against the `AdapterMapping` descriptor before
    `protojson` loads it; an unknown field or a wrong type is refused,
    naming the file and line.
  - Per kind, items read, observations sent and dropped fields are
    metrics, listed in `docs/telemetry.md`. A dropped field is labelled by
    its schema path in the pinned spec, with map keys as `*`, or `other`.
    A fixture with an unknown field and a canary map key yields only
    labels from that fixed set.
  - Only `GET`/`QUERY` (and a `POST` the adapter mapping names) are
    called.
  - With `metadata.labels.team` listed, the Acme `cost-center` label's
    value never leaves the process.
  - A cross-origin paging `Link` stops the kind's sync, and the other host
    gets no request and no token. So do an `https`→`http` downgrade and a
    cross-origin redirect. The log omits the URL's query and userinfo.
  - A `spec.json` that doesn't match `spec.lock`, or a missing or
    unreadable directory, fails at adapter start, naming the path.
  - The threat model gains a control for adapter mappings: the method
    rule, the field check after middleware and the origin rule. For the
    directory checks it cites C-OPS-3, which O extends.
  - Only listed fields leave the process, checked after middleware.
  - No core term is emitted, and views merge into one kind.
  - The Acme secret canaries never appear (`testkit.AssertNoLeaks`).
  - A relation a middleware adds without declaring it goes out proposed.
  - An embedded `$ref` is asserted only when it carries the target's key;
    a reference guessed from a name (`owner_id`) is proposed.
- **O**
  - A branch, tag or short SHA is refused, and one changed byte stops
    boot.
  - git runs with an argument list and `--`, never a shell. Only `https`
    and `ssh` URLs are accepted, with no userinfo. There's no terminal
    prompt (`GIT_TERMINAL_PROMPT=0`) and a timeout applies.
  - `SourceSpec.adapter_mapping` is resolved against the settings file's
    directory, cleaned, and refused if it's a URL or not a directory.
  - The directory and each of its three runtime files, checked as opened
    after symlinks are resolved, fail C-OPS-3's check when the owner isn't
    the service user or root, or when group or others can write them. The
    error names the path. O extends C-OPS-3's wording in the threat model
    to cover adapter mapping directories.
  - A Source that sets `adapter_mapping` for an adapter whose `Describe`
    doesn't take one, or leaves it unset for one that does, is refused at
    boot, naming both.
  - Term rows record repo, commit, checksum and the check that passed.
  - A pinned namespace lets its named adapter claim it.
  - O adds the alias setting and gives `vocab.NewNames` its first real caller.
  - `matching.half_life` keys resolve through `Names`.
  - Every `term.namespace` has a `prefix` row.
  - No configured row lives in the core namespace once EntityKind terms move to the operator module.
- **A**
  - `Describe` returns each module, and suggestions are stored proposed.
  - A second adapter claiming a namespace is refused unless pinned.
  - A module defining a core term, or narrowing a stored shape, is
    refused, naming the source.
  - GitHub and authentik emit only their own terms.
  - The five questions still answer from the demo's mappings and rules,
    in `internal/e2e` and in the Compose demo.
  - Adapter-declared predicates move from bare names in core to their adapter's namespace.
  - The namespace ownership check doesn't trust `term.namespace` for bare rows.
- **I**
  - `init` writes ADR 14's directory from trimmed Kubernetes and GitHub
    spec fixtures. Only identity fields are ticked, secret-looking ones are
    flagged, and there are TODOs for what it can't guess. The first line
    names the schema.
  - `check` runs with no network. It fails an adapter mapping that names a
    route or field the spec lacks. It also fails one that lacks a required
    `reason:` (a secret-looking field, a whole map or a named `POST`), or
    whose `reason:` is empty or `TODO`. A `spec.json` whose sha256 differs
    from `spec.lock` fails, naming both. `init` never writes a `reason:`.
  - Secret-looking names match case-insensitively on each path segment,
    after splitting camelCase, `-` and `_`, so `apiKey`, `x-api-key` and
    `clientSecret` all hit.
  - `init --spec <url>` refuses a URL with userinfo or a query string, and
    takes auth from an env var named on the command line. The fetch uses
    `otelhttp` with a timeout and a size limit.
  - `test` replays `fixtures/` with no network and diffs against
    `testdata/`; `-update` rewrites the golden.
  - `--explain` names each field's term and each dropped field, and its
    output passes `AssertNoLeaks`.
- **K**
  - `bearing adapter check` and `test` pass on `adapters/kubernetes/`.
  - The fixtures contain every Acme canary, and a test asserts it. The
    `test` output, the goldens and `--explain` all pass `AssertNoLeaks`
    against the fakes' full canary list, including `CanaryURLPassword`,
    which the env middleware reads.
  - `middleware.go` embeds exactly `adapter_mapping.yaml`, `spec.lock` and
    `spec.json`.
  - The generic adapter's five answers match the hand-written adapter's
    golden, recorded after A merges and before the switch.
  - The hand-written adapter is then deleted.
- **docs**
  - Say where short names resolve (`entity_kinds` until O, then wherever O's config takes them).

## 6. Decided, and left out

**Decided**

- The foundation is two PRs, F1 and F2, each with a caller and each fitting one review round.
- Mapping format, home and review path: ADR 14. Each adapter
  is one directory made by `bearing adapter init`, the same layout for
  Bearing's adapters and an adopter's. Bearing's are reviewed like code.
  Specs, or trimmed copies, are committed beside a sha256 lock. `test
  --record`, a hosted schema and a spec cache are in ADR 14's "Later".
- GitHub and authentik stay hand-written. ADR 12 puts GraphQL first for
  GitHub, its webhooks already work, and the MVP gains nothing.

**To ROADMAP "Later"**, each with what brings it back:

- Deprecation (`replaced_by`, `consider`) and proposed closes
  (`fact_close`): when a source deprecates a term, or an adapter emits
  evidence that closes another adapter's fact.
- YAML mapping packs: when there's a second instance.
- Webhooks from the spec (`Handle` in the generic adapter): when GitHub
  moves to it.
- From ADR 14: `bearing adapter test --record` (when an author needs
  fixtures from a real API), a schema hosted at `bearing.dougmccall.com`
  (when the docs site serves releases), and a spec cache shared across
  adapters (when committed specs get too large).
- From ADRs 6–11:
  - the model-backed matcher (needs its own ADR) and embeddings;
  - scoring and decay for proposed facts. D's `guess` rules are their
    first writer, stored and shown as proposed, without decay;
  - half-life per predicate;
  - the adapter bundle's suggested config snippet;
  - the HTTP MCP endpoint;
  - ADR 6's "lost for now" items (two time axes, merge reviews,
    data-quality reports, backup and restore), which ADR 6 asked to
    record and nobody did.
- Reloading the API listener's certificate (ADR 9) is already in
  `docs/edge-cases.md`.

## 7. Rules for every agent

- Work in `.worktrees/<stream>`, on a branch named `<type>/<slug>`
  (`feat/vocab-foundation`).
- Read the spike for ideas. Never cherry-pick it.
- `make check` must exit 0 before each commit; gate on `$?`, never on
  piped output.
- Commits are signed through 1Password. If signing fails, stop and
  report; never bypass it.
- No upgrade path for existing data: nothing is deployed.
- No vendor names outside adapters and their tests.
- Docs come last, once behavior has settled.
