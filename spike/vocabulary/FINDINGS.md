# Spike findings: ADR 13, vocabulary as data

Branch `spike/vocabulary`, never merged. Plan and invariants:
[PLAN.md](PLAN.md). Links are pinned to
[`7487a31`](https://github.com/dhm116/bearing/tree/7487a31a40c6f8559990b53fc3d80990860d6ff7),
the reworked tip with every stream merged.

**Verdict: ADR 13 works as written.** Each part has an end-to-end test on
the Acme fakes that checks the answer and its evidence.

Every finding below was checked against the ADR's text before it was
filed:
- **Spike defects** break something the ADR already says.
- **Deviations** are shortcuts this spike knowingly took.
- **Open questions** are things the ADR doesn't answer.

After that check, the ADR needed one addition (what closes a rule's fact)
and three smaller answers (below). ADR 13 was amended with them in
`208c0bb`. The Sprig numbers support copying: 25 functions in 74 lines.
Round 2 (below) built the items round 1 left out.

## The first pass, and why it was redone

The first findings (at `16cbeb4`) listed eight "changes ADR 13 needs".
Most of them were defects in the spike, not gaps in the ADR. Two
shortcuts broke ADR decisions:

| Shortcut | ADR text it broke | What it caused |
| --- | --- | --- |
| Module terms loaded into ADR 10's registry, which holds only kinds | "Every term has a namespaced name"; "Shapes and growth are per term" | attributes stayed one flat list (`tags` had to become `monitor_tags`); predicates made by rules went unchecked |
| The Kubernetes adapter's namespaced mode still sent `owned_by`, `name` and its snapshot of core predicates | "Adapters never emit core terms" | the adapter's next sync closed rule-made facts; `deployed_to` and `defined_in` failed validation and were dropped |

PLAN.md now opens with six invariants, and every agent brief was checked
against them. The rework (`778531c` onward) follows them.

## What works

| Part | End-to-end test | Shows |
| --- | --- | --- |
| Foundation | [`TestVocabFoundationAcme`][t1] | every kind, attribute and relation the adapter sends is an `io.k8s` term; "who owns payments-api" answers `payments` from the operator's `r-team` rule, and the answer names `r-team@1` and the observation |
| Adapter module | [`TestVocabModuleAcceptMappingsAfterBoot`][t2] | the module comes from `Describe`, and its suggestions are stored as proposed. Before they are accepted, payments-api is no Component and has no `defined_in`; billing-api has no `depends_on`. After they are accepted and Bearing reboots on the same store, all three answer. The stored predicate stays `io.k8s:depends_on`, read as `depends_on` through `exact_match`. **ADR 12's "extends later" blocker is gone.** |
| Shape check at boot | [`TestVocabModuleRefusesShapeMismatchAtBoot`][t2b] | an `exact_match` whose domain isn't yet broader than the core's fails boot, naming the mapping |
| Derive rules | [`TestVocabDeriveTeamLabel`][t3], [`…Changes`][t3b], [`…BadTemplate`][t3c] | rules run inside ingest; a re-sync keeps the fact (same id and start); @2 replaces @1 and the fact gains a source; @3 keys another team, so its fact opens and @2's closes; a label change closes the old fact; no label, no fact; **a deleted workload's rule-made fact closes on the adapter's next complete sync**; a bad template fails, naming the rule |
| Operator vocabulary | [`TestVocabOperatorModule`][t4] | `com.acme` pinned to a 40-hex SHA plus a sha256; term rows record repo, ref, sha256 and the check that passed; a one-byte change, a branch name, a short SHA, or no way to verify each stop boot |
| Coverage trail (stretch) | [`TestVocabCoverageTrail`][t5] | per-step proposed facts from `guess` rules over namespaced attributes (`com.datadoghq:tags`); the workload → repo hop through `io.k8s:repository` `exact_match` `defined_in`; a proposed `com.acme:covered_by` scored per claim; a stale handle reports a named break |

[t1]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_foundation_test.go#L17
[t2]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_module_test.go#L60
[t2b]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_module_test.go#L161
[t3]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_derive_test.go#L45
[t3b]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_derive_test.go#L130
[t3c]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_derive_test.go#L190
[t4]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_operator_test.go#L21
[t5]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/e2e/vocab_trail_test.go#L163

How the pieces fit:

```
Kubernetes adapter (io.k8s only)          operator settings and accepted suggestions
  io.k8s:Deployment ────────────────broader────────────► core:Component
  io.k8s:depends_on ────────────exact_match────────────► core:depends_on   (read at query time)
  io.k8s:repository ────────────exact_match────────────► core:defined_in
  io.k8s:labels ── derives r-team@2 'kubernetes:team/{{ .labels.team | lower | trim }}'
                                     │  (runs inside Ingest)
                                     ▼
                     fact  payments-api owned_by payments
                     fact_source: observation + rule r-team@2
                     source: kubernetes-acme → closes on its next complete
                                               sync if the workload is gone
```

Where the code is:
- `exact_match`: [`Registry.ExactMatch`][c1] and the read path in
  [`read.go`][c2];
- the adapter's rewrite to `io.k8s` terms: [`namespaced`][c3];
- rules at ingest: [`derive`][c4];
- rule versions: [`PutMapping`][c5].

[c1]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/pkg/model/vocabulary.go#L346
[c2]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/duckstore/read.go
[c3]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/adapters/kubernetes/kubernetes.go#L94
[c4]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/duckstore/derive.go#L25
[c5]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/internal/duckstore/vocab.go#L98

## Numbers

These are round 1's, after the rework. Round 2 added about 2,580 lines
more, excluding `gen/`.

| What | Value |
| --- | --- |
| Production Go added | about 1,790 lines: `pkg/vocab` 935, store 520, adapter 145, fakes 115, `pkg/model` 70 |
| Tests added | about 1,840 lines |
| Other | `core.yaml` 361 (generated), SQL 49, PLAN and FINDINGS |
| Existing code changed | additive: Kubernetes adapter +82/−11 (namespaced mode, off by default), store read and ingest about +50, `pkg/query` and `pkg/graph` +9 (`Rules` on a fact). No existing test or golden changed meaning. |
| New modules | none (`go.yaml.in/yaml/v3` was already in the graph through testify) |
| Test time | the nine vocab e2e tests run in 1.3s as a package; the slowest is 0.69s (three boots and four syncs) |
| **Sprig subset** | **25 functions, 74 lines** ([funcs.go][f]), standard library only, MIT notice kept |
| Git commit check | about 12 lines: the file's git blob hash, compared with an injected git runner |

[f]: https://github.com/dhm116/bearing/blob/7487a31a40c6f8559990b53fc3d80990860d6ff7/pkg/vocab/funcs.go

**For the Sprig ruling.** The ADR estimates 55 functions and about 460
lines. The rules in this spike needed 25:
- strings: lower, upper, trim, trimPrefix, trimSuffix, replace, contains,
  hasPrefix, hasSuffix, trunc, quote;
- lists: splitList, join, first, last;
- the rest: default, b64enc, b64dec, sha256sum, toJson, kebabcase,
  snakecase, regexMatch, regexFind, regexReplaceAll.

Importing Sprig would add 10 modules and about 10,400 lines. **Copy**, and
add functions when a rule needs one. One fix is due: the list functions
take `[]string`, but attribute lists arrive as `[]any`, so the trail went
through `toJson | regexFind`.

## Spike defects found and fixed

All three break text the ADR already has.

1. **Module terms were loaded into a registry of kinds only.** The ADR
   says "Every term has a namespaced name." Fixed in `778531c`: module
   relations and attributes are now namespaced predicates with their own
   shapes.
2. **The adapter sent core terms.** The ADR says "Adapters never emit core
   terms." Fixed in `778531c`, along with the snapshot closing that it
   caused.
3. **Old rule versions never retired.** The ADR says "Editing a rule is a
   new version." The store kept v1 running beside v2, so a v3 with a new
   key could never close v1's fact. Fixed in `8dce17e`: an asserted
   version closes the asserted versions before it.

## Round 2: the items that weren't built (2026-11-15)

After ADR 13 was amended (`208c0bb`), four streams built what round 1
left out. Each was right-sized: Sonnet for the proto, CLI and Kev work,
Opus for the proposed close. When a stream raised a question, I settled it
together with a ponytail review, choosing the option we both favoured.
Links are pinned to [`b7d1ca8`](https://github.com/dhm116/bearing/tree/b7d1ca82cf5125f3af31bf0e823da95c96505518).

| Item | Where | Result |
| --- | --- | --- |
| `VocabularyModule` in `DescribeResponse`, module version on observations | [`pkg/vocab/proto.go`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/pkg/vocab/proto.go), `proto/`, [`75_module_version.sql`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/internal/duckstore/75_module_version.sql) | The module travels over the real gRPC Describe call. The core sets the owner itself and marks suggestions proposed, whatever the adapter sends (tested with an adapter that claims to be core). Each observation stores `io.k8s@<version>` outside its payload and id. The protocol version stays 1. |
| `bearing vocab review\|accept\|reject` | [`cmd/bearing/vocab.go`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/cmd/bearing/vocab.go), [`pkg/server/vocab.go`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/pkg/server/vocab.go) | Mirrors `bearing link`: API, then the event log (a `MappingDecided` proto event), then the worker, then the store. Accept runs the boot check first and refuses with, for example, "cannot accept k8s-depends-on-is-depends-on yet: accept k8s-daemonset-is-component, … first". The worker checks again before applying. |
| Real git runner for the operator commit check | [`pkg/vocab/git.go`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/pkg/vocab/git.go) | `git -C <dir> rev-parse --verify <ref>:<path>`, tested against a temporary repo. Refuses refs that start with `-`, and paths that are absolute or leave the repo. |
| Proposed close from other adapters' evidence | [`internal/duckstore/close.go`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/internal/duckstore/close.go), [`TestVocabCloseFromPullRequest`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/internal/e2e/vocab_close_test.go) | A merged PR that deletes payments-api's manifest proposes closing its rule-made `owned_by` fact, stored as a `fact_close` record shaped like `identity_link`. The fact is untouched until a person confirms. Confirming closes it at the merge time and records the actor and reason. Rejecting holds, and the same PR never proposes again. A Kubernetes observation after the merge contradicts the proposal, and confirming is then refused. |
| Kev System One judgement on the trail's pages step | [`pkg/systemone`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/pkg/systemone/client.go), [`trail_model.go`](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/pkg/vocab/trail_model.go), [live test](https://github.com/dhm116/bearing/blob/b7d1ca82cf5125f3af31bf0e823da95c96505518/pkg/vocab/trail_model_live_test.go) (`-tags kev`) | One multiple-choice question per broken step, over the observed PagerDuty services plus "none". The step's p multiplies its strength and is never a deposit (ADR 7: "nothing Bearing inferred or answered can reinforce itself"). |

**Calling a real Kev changed the design.** The first version asked a yes/no
question per service, and its tests faked p = 0.9 for a match and 0.1 for a
mismatch. Kev-4B, run locally, answered yes to nearly everything: 0.58 for
`@pagerduty-payments` against Checkout Platform, 0.65 for an unrelated
service, and about 0.8 for everything under stricter wording. A
multiple-choice question with a "none" option does tell them apart. Live
results with three services:

| Handle | Kev picked | p |
| --- | --- | --- |
| `@pagerduty-checkout-platform` | Checkout Platform | 0.78 |
| `@pagerduty-checkout` | Checkout Platform | 0.61 |
| `@pagerduty-payments` | Payments | 0.70 |
| `@pagerduty-search` | none | 0.70 |
| `@pagerduty-checkout-old` (stale) | Checkout Platform | 0.56 |

At a 0.5 threshold the stale handle joins. A model can only judge how
similar two names are, not whether a handle still exists, so a stale
handle needs evidence (PagerDuty's list of live integration handles), not
a higher threshold. The threshold must also be calibrated on labelled
examples before it decides anything (ADR 7).

## Deviations this spike keeps

None of these is an ADR finding; each is a shortcut.

- **Core terms keep their bare stored names** (`owned_by`, `Component`).
  The ADR says "Stored names are always full." Bare names act as the one
  fixed alias of `core`, which kept the five questions' code unchanged.
- **`kind` and `kind_predicate` stay beside `term`, `prefix` and
  `mapping`.** The ADR says the new tables replace them.
- **No boot path loads vocabulary modules.** `pkg/server/boot` builds the
  server with no modules or mappings. The end-to-end helper and the
  `cmd/bearing` tests set them directly, and the vocab API answers 501 in a
  real server.
- **The close and trail proposers run as passes that tests call.** They
  aren't wired into ingest or the server, and closes have no CLI.
- **`core.yaml` lives in `pkg/vocab/`** so it can be embedded; the ADR
  puts it in `vocab/`.

## What ADR 13 needs

1. **What closes a rule's fact (Doug, 2026-11-15).** The ADR covers
   versions ("Only facts that differ close or open"). It doesn't cover a
   fact whose subject goes away. Proposed text:
   - A fact a rule made keeps its observation's source.
   - When no webhook reports the change, it closes on evidence: either the
     observation's own adapter reports the subject gone on its next sync
     (a complete snapshot, or an audit log it reads), or other adapters'
     observations support closing it, such as a PR that removes the
     Deployment.
   - The second kind is inferred, so it only proposes the close, as a
     trail does.

   The spike builds the first kind.
2. **When an `exact_match` is checked.** The ADR says the shapes are
   "checked at boot". If the operator accepts `io.k8s:depends_on
   exact_match depends_on` before the workload kinds are broader than
   Component, the next boot fails. A mapping decision shouldn't be able
   to stop Bearing. Proposed text: `bearing vocab accept` refuses a
   mapping that wouldn't pass the boot check, naming what it needs
   first, and the boot check stays as the backstop.
3. **Several broader terms.** The ADR says "`extends` becomes a dated
   `broader` row." It doesn't say whether a term may have more than one.
   SKOS allows several. The registry allows one parent, so the spike
   refuses a second and says why. The ADR should rule on it.
4. **Which check `verified_by` records.** The explainer says each term
   row records "which check passed". When both the commit check and the
   checksum check run, the spike records `sha256`. The ADR should say
   whether to record both (`commit+sha256`).

## For the trail ADR (still open in ADR 13)

- **Independence is per claim** (subject, predicate, object), after a
  record that describes another (Terraform's copy of a monitor) is folded
  onto it. Each step's strength is n/(n+1), and the steps multiply:
  - Datadog only: 0.25.
  - Adding the Terraform config: still 0.25.
  - Adding Terraform's repo reference, a second claim: about 0.33.
- **The joined link needs a term.** The spike puts `covered_by` in the
  operator's namespace (`com.acme`), not PagerDuty's, because the server's
  joiner makes it. That works, but it means an operator module declares a
  term the server writes. The trail ADR should choose between that, a core
  term, and reusing `on_call_for`.
- **Name resolution.** A Datadog `service:` tag names a service, not a
  workload. The fake monitor carries `kube_cluster_name`, `kube_namespace`
  and `kube_deployment` tags so a rule can build the workload key, and the
  key only handles Deployments. A real trail needs a step that resolves a
  service name to a Component, which is where a Kev System One judgement
  belongs.
- **A model judgement is a feature, not evidence.** The step's p multiplies
  its strength. Ask one multiple-choice question per broken step, over the
  observed records plus "none", not one yes/no question per record (see
  round 2). Calibrate the join threshold on labelled examples, and detect a
  stale handle with evidence rather than the model.
- **A proposed close is its own record.** It is dated and attributed,
  shaped like `identity_link`, and sits beside the fact. Only a person
  confirms it, and the close takes effect at the time the evidence names,
  such as the merge. The fact's own source observing the subject later
  contradicts the proposal. "The same evidence" means the same fact and the
  same cause record. The step needs an operator or core term (`removes`
  here), the same naming question as `covered_by`.
- **Not built:**
  - closing trails that stop joining;
  - combining several monitors;
  - decay or contrary evidence;
  - Kev on the service-name → Component step;
  - a confirmed close can't be undone.
