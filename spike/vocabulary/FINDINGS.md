# Spike findings: ADR 13, vocabulary as data

Branch `spike/vocabulary`, never merged. Plan: [PLAN.md](PLAN.md). All
links below are pinned to
[`3f10827`](https://github.com/dhm116/bearing/tree/3f108271a56a339b1b5e0ea465f18a99d0236d6d),
the tip with every stream merged.

[B]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d

**Verdict: the ADR works as written for items 1–4.** Each item has an
end-to-end test on the Acme fakes that checks the answer and its evidence.
The spike found eight concrete gaps for the ADR to close (listed at the
end), and none of them is a reason to reject it. The cheapest Sprig
option turned out smaller than the ADR estimated: 25 functions in 74
lines.

## What worked

| Item | End-to-end test | Shows |
| --- | --- | --- |
| 1. Foundation | [`TestVocabFoundationAcme`][t1] | core module and `io.k8s` loaded into today's registry; payments-api stored as `io.k8s:Deployment`; owner answer and evidence unchanged; `fact_source.rule` is `''` for stated facts |
| 2. Adapter module | [`TestVocabModuleAcceptBroaderAfterBoot`][t2] | the module comes from `Describe`; its suggestions are stored as proposed; accepting `k8s:Deployment broader core:Component` after first boot reboots without refusal, and "who owns payments-api" answers `payments` with evidence. **ADR 12's "extends later" blocker is gone.** |
| 3. Derive rules | [`TestVocabDeriveTeamLabel`][t3] | `team:{{ .labels.team \| lower }}` makes owned_by, citing `r-team@1`; v2 keeps the fact id and `valid_from` and adds a source row; a version making a different fact closes the old one; no label means no fact; a bad template fails compile, naming the rule |
| 4. Operator vocabulary | [`TestVocabOperatorModule`][t4] | `com.acme` from a file pinned to a 40-hex SHA plus a sha256; term rows record repo, ref, sha256 and `verified_by`; a one-byte change, a branch ref, a short SHA, or no checksum with no git each stop boot |
| 5. Coverage trail (stretch) | [`TestVocabCoverageTrail`][t5] | per-step proposed facts from `guess` rules; a joiner proposes payments-api `covered_by` checkout; the score counts per claim; a stale handle shows a named break |

Unit tests pin the name rules: an ambiguous short name names both
namespaces, an alias wins, `core` is reserved, and `k8s` alone is refused
([vocab_test.go][u1]). They also pin grow-only per term and one owner per
namespace ([duckstore/vocab_test.go][u2]).

[t1]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/internal/e2e/vocab_foundation_test.go#L14
[t2]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/internal/e2e/vocab_module_test.go#L37
[t3]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/internal/e2e/vocab_derive_test.go#L65
[t4]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/internal/e2e/vocab_operator_test.go#L21
[t5]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/internal/e2e/vocab_trail_test.go#L133
[u1]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/pkg/vocab/vocab_test.go
[u2]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/internal/duckstore/vocab_test.go

## Numbers

| What | Value |
| --- | --- |
| Lines added, production Go | about 1,500 (store 460, `pkg/vocab` 840, adapter 90, fakes 100) |
| Lines added, tests | about 1,600 |
| Other | `core.yaml` 361 (generated), SQL 49 |
| Existing code changed | Kubernetes adapter +44/−7 (namespaced mode, off by default); about 12 lines in `pkg/model`, `ingest.go`, `vocabulary.go` and `10_graph.sql`; no existing test or golden changed meaning |
| New modules in `go.mod` | none (`go.yaml.in/yaml/v3` was already in the graph through testify; now direct) |
| Test time | the six vocab e2e tests take 0.03–0.32s each, and the e2e package run takes 0.9s; `pkg/vocab` unit tests 0.2s |
| **Sprig subset** | **25 functions, 74 lines** ([funcs.go][f]), standard library only, MIT notice kept ([SPRIG-LICENSE][fl]) |
| Git commit check | about 12 lines: the git blob hash of the file, compared with a git runner that tests inject |

[f]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/pkg/vocab/funcs.go
[fl]: https://github.com/dhm116/bearing/blob/3f108271a56a339b1b5e0ea465f18a99d0236d6d/pkg/vocab/SPRIG-LICENSE

**Sprig ruling input.** The ADR estimated 55 functions and about 460
lines. The rules here needed 25 functions in 74 lines:
- strings: lower, upper, trim, trimPrefix, trimSuffix, replace, contains, hasPrefix, hasSuffix, trunc, quote;
- lists: splitList, join, first, last;
- the rest: default, b64enc, b64dec, sha256sum, toJson, kebabcase, snakecase, regexMatch, regexFind, regexReplaceAll.

Every Acme rule in this spike was written with them. Importing Sprig
would cost 10 modules and about 10,400 lines. **Copy.** One fix is due:
the list functions take `[]string`, but attribute lists arrive as
`[]any`, so the trail had to go through `toJson | regexFind`.

## Surprises

1. **A derived fact is closed by its adapter's next snapshot.** The derived
   fact carries the observation's source. The adapter's next full sync
   closes every `owned_by` fact of that source it didn't touch, and the
   derived fact is one of them. Derive then reopens it with a new id. So
   supersession (v1 → v2 keeps the fact) holds only if nothing re-ingests
   between versions. The test avoids that case.
2. **An unmapped term can't hold core relations.** An `io.k8s:Deployment`
   with no broader mapping fails validation for `deployed_to`,
   `defined_in` and `depends_on`, whose domain is Component, and for the
   core `name` attribute. Namespaced mode drops them, and it still drops
   them after the mapping is accepted. So the workload → repo hop that
   the PagerDuty trail relies on is missing.
3. **Attributes are still a flat namespace.** `vocab.Registry` adds kinds
   only. Module attribute and relation terms never reach the registry:
   - Derived predicates such as `com.datadoghq:watches` aren't checked
     against a domain or range.
   - An adapter can't reuse a domain-limited core attribute name: the
     trail had to rename `tags` to `monitor_tags`.
   This is the ADR 12 collision, moved rather than removed.
4. **Mapping checks span two layers.** An adapter module can check only
   that its suggestions end outside its own namespace. The rule that
   they end at a core or operator term needs every loaded module, so it
   lives in `Registry`.
5. **The registry has single inheritance.** Two asserted broader mappings
   from one term are refused. SKOS allows several broader terms; Bearing
   can't, without a registry change.
6. **The wire format isn't defined yet.** The module crosses the wire as
   the Go struct. The `VocabularyModule` proto message is still needed,
   and the server must set `Owner` itself rather than trust it from the
   module.
7. **Parallel worktrees share golangci-lint's lock.** Two streams
   collided on "parallel golangci-lint is running". Worth knowing for the
   next fan-out.

## Changes ADR 13 needs

1. **Say which source a derived fact carries.** Either a rule source such
   as `rule:r-team`, closed only by its own rule, or derive runs in the
   same pass as ingest and counts as touched. The spike recommends the
   rule source: it keeps rule facts out of the adapter's snapshot and
   makes "a fact closes when its rule stops making it" the only closing
   rule.
2. **Register relation and attribute terms, not just kinds.**
   - Namespace attribute names in the registry, so `github:id` and
     `pagerduty:id` are different predicates (today they still share
     `id`).
   - Check derived predicates against their domain and range.
3. **Let an `exact_match` or `broader` mapping carry core relations.**
   Once `k8s:Deployment broader core:Component` is accepted, the adapter's
   `deployed_to` and `defined_in` should validate. That needs either
   validation that reads the booted registry (it does) and an adapter
   that emits those relations in namespaced mode, or a rule that core
   relations on a module term are held as proposed until a mapping makes
   them valid.
4. **Spell out what a rule sees.** A rule sees the entity's attributes,
   its `key` and its `kind`:
   - A missing field or an empty render makes no fact; any other
     template error fails the rule.
   - Only asserted, open rules run.
   - A rule whose key uses no field renders for every matching entity.
5. **Say how operator modules are verified.** `ref` is 40 lowercase hex
   characters, and the commit check compares the git blob hash; it
   proves the file matches the commit, not that the commit is the right
   one. `verified_by` records every check that passed (`commit+sha256`).
   `path` is repo-relative and serves both the read and the git lookup.
6. **Say which layer checks which mapping rule.** The adapter module
   checks its own namespace; the registry checks the core-or-operator
   end. Name both, and say that a repeated suggestion never reopens a
   decided mapping.
7. **Allow several broader terms, or say why not.** SKOS allows several;
   the registry's single parent refuses them.
8. **Record the Sprig subset.** Copy, with the 25 functions above,
   list functions over `[]any`, and the MIT notice.

## For the trail ADR (still open in ADR 13)

- Independence is per claim (subject, predicate, object), after folding
  a "describes" record (Terraform's copy of a monitor) onto what it
  describes:
  - Datadog only: 1/2 × 1/2 = 0.25.
  - Adding the Terraform config: 0.25, unchanged.
  - Adding Terraform's repo reference, a second claim: 2/3 × 1/2 ≈ 0.33.
- Each step's strength is n/(n+1) (ADR 7's Strength with a prior of 1),
  and the steps multiply.
- The joined link is a proposed fact (source `trail`, rule
  `trail-coverage@1`): its confidence is the score, its qualifiers name
  the path, and `fact_source` lists every observation on the path. Facts
  fit because it relates entities; identity links (not `same_as`) and
  mappings (terms) don't.
- The trail is anchored on the repo, because a Datadog tag doesn't carry
  the workload's cluster and namespace. The ADR needs a step that
  resolves a service name to a Component.
- `covered_by` has no natural owner. Choose core `covered_by`, or reuse
  `on_call_for` with a wider domain.
- Skipped, and listed for that ADR:
  - closing trails that no longer join;
  - combining several monitors for one service;
  - decay and contrary evidence;
  - the Kev System One feature, which would judge whether
    `@pagerduty-checkout` is the service "Checkout Platform".

## Deviations from the ADR in this spike

- `kind` and `kind_predicate` stay beside the new tables. The Kubernetes
  adapter's namespaced mode is off by default, so existing tests are
  untouched.
- `core.yaml` lives in `pkg/vocab/`, not `vocab/`, because `go:embed`
  can't reach a parent directory.
- There is no proto `VocabularyModule`; `DescribeResult` carries the Go
  struct.
- The real git runner for the commit check isn't written; tests inject
  one.
- No `bearing vocab review|accept|reject` commands: the tests call
  `DecideMapping`.
- The coverage gate was skipped for stream commits after the foundation
  (Doug: it belongs to PR workflows,
  [dhm116/bearing#325](https://github.com/dhm116/bearing/pull/325)).
