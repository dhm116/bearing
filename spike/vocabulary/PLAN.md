# Spike plan: ADR 13, vocabulary as data

Branch `spike/vocabulary`, never merged. The spike shows ADR 13
([`docs/adr/0013-vocabulary-as-data.md`](../../docs/adr/0013-vocabulary-as-data.md))
working on the Acme fakes, so the ADR can be accepted or amended with
evidence. AGENTS.md still applies: fast tests, faked systems, DuckDB
`:memory:`, `make check` green before every commit.

## Shape of the spike

It is built **beside** today's code, not in place of it. The core module
loads into today's `model.Registry`; the Kubernetes adapter gains a
`namespaced` mode and keeps its default behaviour, so every existing test
and golden stays as it is. Deviations from the ADR are listed in
FINDINGS.md, not hidden.

```
pkg/vocab/            terms, modules, names, mappings, registry bridge   (item 1)
  core.yaml           the core module (ADR says vocab/core.yaml; see below)
  derive*.go funcs*.go  derive rules and the copied Sprig subset          (item 3)
  operator*.go        operator module, pinned and verified                (item 4)
  trail*.go           coverage-trail joiner                               (item 5)
internal/duckstore/
  70_vocab.sql vocab.go  term, prefix, mapping tables                     (item 1)
  derive*.go          derived facts with fact_source.rule                 (item 3)
adapters/kubernetes/  namespaced emission (item 1), module in Describe (item 2)
internal/e2e/vocab_*_test.go  one end-to-end test file per item
```

`core.yaml` sits in `pkg/vocab/` because `go:embed` can't reach a parent
directory; a root `vocab/` package just to embed one file isn't worth it.

## Item 1: foundation (lead, lands alone)

**Files:** `pkg/vocab/{vocab.go,names.go,registry.go,core.yaml}` and tests;
`internal/duckstore/{70_vocab.sql,vocab.go}` and test; `10_graph.sql`
(`fact_source.rule`), `ingest.go` (column list), `vocabulary.go` (no parent
refusal for namespaced kinds); `pkg/model/vocabulary.go` (namespaced kind
names); `adapters/kubernetes/kubernetes.go` (`namespaced` mode);
`internal/e2e/vocab_foundation_test.go`.

**Interfaces** (`pkg/vocab`):

```go
const CoreNamespace = "com.dougmccall.bearing.core"
type Term struct{ Name, Is, Type, Cardinality, Conflict string; Domain, Range []string; Deprecated *Deprecation }
type Module struct{ Namespace, Short, Version string; Source Source; Terms []Term; Suggests []Mapping }
type Source struct{ Repo, Path, Ref, SHA256, VerifiedBy string }
type Mapping struct{ ID string; Version int; Subject, Predicate, Object string; Target *Target
    Status, Author, Justification string; ValidFrom time.Time; ValidTo *time.Time }
type Target struct{ Kind, Key string }       // a derives rule's object entity
func Parse(b []byte) (Module, error)         // YAML, validated
func Core() Module                           // the embedded core module
func NewNames(namespaces []string, aliases map[string]string) (*Names, error)
func (n *Names) Resolve(name string) (string, error) // short or full -> full
func KindOf(full string) model.Kind          // core terms -> bare core kinds
func Registry(base *model.Registry, mods []Module, broader []Mapping) (*model.Registry, error)
```

Store (`internal/duckstore`): `SyncModule(ctx, vocab.Module, now)`,
`PutMapping(ctx, vocab.Mapping)`, `DecideMapping(ctx, id, status, author,
why, now)`, `Mappings(ctx, at)`.

**Rules and checks:**

| Rule | Check |
| --- | --- |
| Namespaces are reverse domains; `k8s` alone is refused | `TestNamesRefuseBareNamespace` |
| Short name by last segment; a second namespace makes it fail, naming both | `TestNamesAmbiguousShortNameNamesBoth` |
| An operator alias wins over the short name | `TestNamesAliasWins` |
| `core` is reserved: no alias, and `com.example.core` doesn't make it ambiguous | `TestNamesCoreReserved` |
| `core.yaml` matches today's registry one to one | `TestCoreModuleMatchesRegistry` |
| A term only grows: a changed shape or a dropped term fails boot, naming it | `TestSyncModuleGrowOnly` |
| `fact_source.rule` exists and adapter facts carry `''` | e2e below |
| A broader mapping added after first boot is not refused | `TestSyncVocabularyAcceptsLaterBroader` |
| **E2E:** Acme Kubernetes in namespaced mode, core + `io.k8s` terms synced: payments-api is stored as `io.k8s:Deployment`, its owner answer and evidence are unchanged | `TestVocabFoundationAcme` |

## Item 2: adapter module (stream A)

**Files:** `adapters/kubernetes/vocab.go` (+ test), `Describe` in
`kubernetes.go`, `pkg/adapter` `DescribeResult.Vocabulary`, optionally
`proto/` + `gen/` (`VocabularyModule` in `DescribeResponse`) if `make
generate` runs offline; `internal/duckstore/vocab_owner*.go` only if a
namespace-owner check needs storage; `internal/e2e/vocab_module_test.go`.

- Describe returns the `io.k8s` module (Deployment, StatefulSet,
  DaemonSet, Namespace, labels, replicas) with suggestions
  `k8s:Deployment broader core:Component` and `k8s:Namespace broader
  core:Environment`. The server stores suggestions as **proposed**.
- The first source to publish a namespace owns it; a second is refused by
  name. A module that defines a core term is refused.
- **E2E `TestVocabModuleAcceptBroaderAfterBoot`:** boot, sync Acme;
  `Find("payments-api", "Component")` finds nothing and the mapping is
  proposed. Accept it (author, justification), reboot on the same store
  (no refusal), and "who owns payments-api" answers `payments` with the
  label observation as evidence; the mapping row shows author, status and
  `valid_from`.

## Item 3: derive rules (stream B)

**Files:** `pkg/vocab/{derive.go,funcs.go,SPRIG-LICENSE}` (+ tests),
`internal/duckstore/derive.go` (+ test), `internal/e2e/vocab_derive_test.go`.

- A `derives` mapping has a Go template key over the observation's
  allow-listed fields (its entity attributes, key and kind). Templates
  parse at boot; a bad one fails, naming the rule. `missingkey=error`;
  an empty render or a missing field makes no fact.
- Functions: a copied, deterministic subset of Sprig with Sprig's names.
  No Sprig dependency. Count the lines (`funcs.go`, non-blank,
  non-comment) for FINDINGS.
- A derived fact cites the rule in `fact_source.rule` as `<id>@<version>`.
  A fixed-field rule asserts; a rule marked `guess` only proposes.
- Supersession: v2 that makes the same fact adds a `fact_source` row; the
  fact keeps its id and `valid_from`. A fact only v1 made closes.
- **E2E `TestVocabDeriveTeamLabel`:** Acme in namespaced mode with the
  adapter's own owner relation off; rule `r-team@1` `team:{{ .labels.team
  | lower }}` makes payments-api owned_by payments, evidence = the
  observation + `r-team@1`. Then `r-team@2` (adds `| trim`) supersedes:
  same fact id, two sources, no close. A workload without the label gets
  no fact. A bad template stops boot.

## Item 4: operator vocabulary (stream C)

**Files:** `pkg/vocab/operator.go` (+ test),
`internal/e2e/vocab_operator_test.go`, `testdata/vocab/acme.yaml`.

- Settings name `com.acme`, repo, path, full 40-hex commit SHA (branches,
  tags, short SHAs refused) and optional `sha256`. Bearing reads the local
  copy and verifies it: by commit when a local git clone can show the blob
  (optional, only if cheap), else by sha256; neither possible or a
  mismatch stops boot, naming the file. Term rows record repo, commit,
  checksum and which check passed.
- **E2E `TestVocabOperatorModule`:** `com.acme` defines `Workload`;
  asserted settings mappings `k8s:Deployment broader acme:Workload` and
  `acme:Workload broader core:Component`; `Find("payments-api",
  "com.acme:Workload")` and the Component query both find it; the term
  rows cite the sha256 check. A file with one byte changed stops boot.

## Item 5: coverage trail (stream D, stretch, after B)

**Files:** `pkg/vocab/trail.go` (+ test), `internal/fakes/coverage.go`,
`internal/e2e/vocab_trail_test.go`.

- Fake observations: a Datadog monitor (tag `service:payments-api`,
  message `@pagerduty-checkout`), a Terraform resource for the same
  monitor that also references the payments repo, and a PagerDuty service
  `checkout`.
- Proposing rules make per-step proposed facts: monitor `watches`
  payments-api, monitor `pages` checkout, terraform monitor `defined_in`
  repo.
- A joiner, in the style of ADR 7's matchers, proposes "payments-api
  covered_by checkout" with its path as evidence. Score per claim: the
  Datadog and Terraform config claims count once; the repo reference
  counts as a second path.
- A stale handle (`@pagerduty-checkout-old`) shows a broken trail: no
  proposal, and the break named.

## Fan-out

Item 1 lands alone on `spike/vocabulary`. Then streams A, B and C run in
parallel, each in `.worktrees/<name>` on `spike/vocabulary-<name>`, owning
only the files listed for it. D starts after B merges. The lead merges
each back onto `spike/vocabulary`. Every agent: `GOTOOLCHAIN=go1.27.4 make
check` must exit 0 before a commit; never bypass signing; stop and report
if signing fails.

## Finish

`spike/vocabulary/FINDINGS.md`: what worked, numbers (lines added, test
time, Sprig subset size), surprises, and concrete ADR 13 changes, with
links pinned to commit SHAs.
