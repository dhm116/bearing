# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository. Read
this first, then the [README](README.md) for the pitch and
[CONTRIBUTING](CONTRIBUTING.md) for the ground rules.

## What Bearing is

An open-source foundation for a developer platform. Small **adapters** read
tools an organization already uses (GitHub first, then AWS and PagerDuty) and
emit **observations**. The core resolves them into **entities** and **facts**
in a graph that is the source of truth, with a semantic (vector) index beside
it. Every component talks through an interface in `pkg/contracts`, and any
backend that passes that interface's conformance suite can replace the
default.

The project is an early scaffold. The spec, protocol and contracts are drafts
at version 0.1 and are expected to change, but change them deliberately (see
[Changing the design](#changing-the-design)).

## Commands

Go 1.27.1 or later; the `go` command downloads the toolchain if needed.

| Command | What it does |
| --- | --- |
| `make check` | Everything CI runs, in order: `generate-check`, `lint`, `vet`, the `tools/` module's tests, `cover`, `covergate`, `build`, `vuln`. CI runs the same steps as parallel jobs (see below), so it finishes sooner than this does |
| `make generate` | After editing `proto/`: `buf format`, `buf lint`, then `buf generate` into `gen/go` and `gen/jsonschema` (buf and plugins pinned in `tools/go.mod`). Commit the output |
| `make generate-check` | Fails if `proto/` isn't formatted or doesn't lint, or if `gen/` differs from what `buf generate` writes. `buf breaking` turns on when the MVP closes |
| `make lint` | golangci-lint (pinned in `tools/go.mod`, config in `.golangci.yml`): gofumpt, goimports, revive, errcheck, errorlint, staticcheck, gosec, forbidigo, depguard, nolintlint |
| `make test` | `go vet ./...` and `go test ./...` (no external services needed) |
| `make cover` | `go test -coverpkg=./... -coverprofile=cover.out ./...` |
| `make covergate` | Fails if under 80% of Go lines changed since the merge base with `origin/main` are covered, or if total coverage is below the merge base's (`cmd/`, `gen/` and generated files excluded). The total check runs for any change outside docs, Markdown, `LICENSE`, `NOTICE` and `.github/` (workflows excepted); a change touching only those skips the baseline run |
| `make vuln` | `govulncheck ./...` (pinned in `tools/go.mod`) |
| `make build` | Builds `bin/bearing` and `bin/bearing-adapter-github` |
| `make fmt` | gofumpt and goimports via `golangci-lint fmt` |
| `make test-surrealdb SURREALDB=ws://127.0.0.1:8000 SURREALDB_USER=root SURREALDB_PASS=root` | SurrealDB suites against a running server (`surreal start --user root --pass root memory`) |
| `make test-embedded SURREALDB_LIB=<dir>` | Same suites against embedded SurrealDB (CGO, needs `libsurrealdb_c.a`) |

Run `make check` before every commit. A single package:
`go test ./pkg/model/ -run TestInvalidObservations`.

SurrealDB tests skip unless `BEARING_TEST_SURREALDB` is set or the binary is
built with `-tags surrealembed`, so a green `make test` does not prove the
SurrealDB backend works. If you touch `internal/surrealstore`, run one of the
SurrealDB targets or say plainly that you could not.

Do **not** run `go mod tidy`: it fails on `surrealdb.c.go`'s test
dependencies. Add dependencies with `go get <module>@<version>` and keep
`go.mod` edits minimal.

## Layout

| Path | Role |
| --- | --- |
| `docs/spec/` | The specification: data model, adapter protocol, component contracts. The spec is the product; code implements it. |
| `docs/adr/` | Architecture decision records, numbered. [`template.md`](docs/adr/template.md) for new ones. |
| `docs/telemetry.md` | Catalog of spans, metrics and telemetry config. Keep it current. |
| `docs/security/threat-model.md` | Trust boundaries, threats and controls (`C-<AREA>-<n>` IDs). Update it when adding an input, a boundary or an `insecure_*` setting. |
| `proto/bearing/{model,event}/v1alpha1` | Protobuf: the only source of truth for the data model, declarations and events ([ADR 6](docs/adr/0006-protobuf-contracts.md)). Linted and formatted by buf (`buf.yaml`). `v1alpha1` until the MVP closes. |
| `proto/bearing/resolver/v1alpha1` | The resolver's private state, stored by `GraphStore` as opaque `Any` values (not part of the data model; add-only until the MVP closes, see `docs/spec/contracts.md`). |
| `gen/go`, `gen/jsonschema` | Generated from `proto/` by `make generate` (`buf.gen.yaml`) and committed; never edit by hand. CI checks they are current. |
| `testdata/observations/` | Example observations in ProtoJSON: `valid/` must validate, each file in `invalid/` must fail for the one reason `pkg/model`'s test table names. |
| `testdata/declarations/` | The spec's reference adapter declarations; tests validate them. |
| `pkg/model` | Helpers around the generated types: the kind and predicate registry, keys, ProtoJSON encoding, validation at the edges (`ValidateObservation`, `ValidateDeclaration`, `ValidateManualEvent`), `FactID` and `ContentHash`. |
| `pkg/adapter` | Adapter protocol: `Adapter` interface, `ServeStdio` for adapter authors, client for the core. |
| `pkg/contracts` | Interfaces between components (`GraphStore`, `VectorIndex`, `EventBus`, `Judge`, …). |
| `pkg/contracts/conformance` | Test suites every backend must pass. |
| `pkg/query` | The CLI's query layer: `get`, `owner`, `related` and `changes` over a `GraphStore`, each answer with its sources, events, confidence and observed times. `cmd/bearing` reads the graph only through it. Not a stable API yet; M3's server will answer the same questions. |
| `pkg/contracts/instrument` | OpenTelemetry wrappers so every backend gets the same spans and metrics. |
| `pkg/store` | Opens graph store and vector index from URLs (`mem://`, `surrealdb+ws://`, `surrealkv://`, …). |
| `pkg/telemetry` | OpenTelemetry setup, `Logger`, `Tracer`, `Meter`, `Fail`. |
| `pkg/clock` | `Clock` interface (now, timers, tickers) that components take instead of package `time`; `Real` wraps `time`. |
| `internal/memstore` | In-memory reference backend for both contracts, and the rule engine other backends run their operations on (`surrealstore` loads rows into a scratch `memstore.Store`). Production code. |
| `internal/surrealstore` | SurrealDB backend (server mode pure Go; embedded mode behind `surrealembed`). |
| `internal/testkit` | Test fakes: `FakeClock` (a `clock.Clock`), `SeqIDs`, script/fixture HTTP servers, fake `Secrets` and `AssertNoLeaks`. Tests only. |
| `internal/fakes` | httptest fakes of source systems for tests and demos: a GitHub API (REST, GraphQL, signed webhook deliveries) and an Authentik-like directory, both serving one fictional org (`acme`) with a scripted timeline (`Story`) and an injected clock. Its recorded directory feed is `testdata/acme/directory.ndjson`. Tests only. |
| `adapters/github` | GitHub adapter, the worked example for new adapters. |
| `cmd/bearing` | Developer CLI: `adapter describe`, `adapter sync` (validates every observation and prints them as ProtoJSON NDJSON), and `get`, `owner`, `related` and `changes`, which read a store given by `--store` or `$BEARING_STORE` through `pkg/query`, optionally `--as-of` and `--recorded-at`. |
| `cmd/bearing-adapter-github` | Binary that serves the GitHub adapter on stdio. |
| `spikes/` | Spike code, each in its own Go module(s) so the root module stays untouched; results in `docs/spikes/`. |
| `tools/` | Separate Go module: pinned golangci-lint, govulncheck, buf, protoc-gen-go and protoc-gen-jsonschema, and the coverage gate (`tools/covergate`). Never imported by Bearing code. |

The Go module path is the placeholder `bearing.example`. Import packages as
`bearing.example/pkg/...`; don't rename the module unless asked.

## Architecture rules

These come from the ADRs and project decisions. Don't break them without a
new ADR.

1. **The graph is the source of truth** ([ADR 2](docs/adr/0002-graph-is-source-of-truth.md)).
   Every vector points at a graph entity and the index can be rebuilt from
   the graph. Ownership and policy answers read only asserted facts; results
   from semantic search are verified in the graph.
2. **Adapters are isolated from the core** and implement one Protobuf
   adapter service: `Describe`, `Sync` (cursor paged) and optional `Handle`
   for webhooks ([ADR 9](docs/adr/0009-wasm-adapters.md)). Once
   [M4](https://github.com/dhm116/bearing/milestone/5) lands, sandboxed
   WASM modules reaching the world only through granted host capabilities
   are the default runtime. Adapters that can't run as WASM run as local
   processes the core starts, serving the same service over Connect/gRPC
   on a Unix socket in a private directory; the core owns their
   lifecycle, environment, secrets and telemetry (ADR 9 A13). The stdio JSON-RPC
   transport ([ADR 3](docs/adr/0003-adapter-protocol.md)) is retired; the
   scaffold's stdio code runs the GitHub adapter until M4 replaces it, and
   nothing new is built on it. Adapters are stateless and read-only
   against their source. WASM adapters never hold credentials; the host
   injects them. The host verifies webhook signatures before a delivery is
   logged, with one verifier per signature scheme that the adapter's
   manifest will declare (field tracked in
   [#58](https://github.com/dhm116/bearing/issues/58)), and WASM adapters
   never see webhook secrets. Ingest stays off until that verifier exists;
   until then the adapter's own verification is the check (ADR 9 A15).
   Adapters never resolve identities across systems. That is the core's
   job.
3. **Replace nothing; integrate.** Bearing adapts existing tools (Backstage
   included) rather than competing with them. Keep adapters small and
   focused on useful data types.
4. **Pluggable backends behind contracts.** Code outside a backend depends on
   `pkg/contracts` interfaces only. Backends are wired up in `pkg/store`,
   wrapped with `pkg/contracts/instrument`, and must pass
   `pkg/contracts/conformance`.
5. **One store to start** ([ADR 5](docs/adr/0005-one-store-to-start.md),
   proposed). SurrealDB serves both `GraphStore` and `VectorIndex` by
   default; `mem://` for tests. The default binary must stay CGO-free and
   free of BSL-licensed code, so anything that imports `surrealdb.c.go` goes
   in a file with `//go:build surrealembed` and gets a stub in a
   `//go:build !surrealembed` file (see `embedded.go` / `embedded_stub.go`).
6. **Few dependencies** ([ADR 1](docs/adr/0001-license-and-language.md)).
   Prefer the standard library. A new third-party dependency needs a reason
   in the commit message, and a significant one needs an ADR. Everything
   compiled into or shipped with Bearing's artifacts must be Apache-2.0
   compatible. Developer tools pinned in `tools/` (a separate module that
   Bearing code never imports, links or distributes) need an OSI-approved
   licence and a note in [`tools/README.md`](tools/README.md); golangci-lint
   is GPL-3.0 on those terms.

## Code conventions

- **Telemetry** ([ADR 4](docs/adr/0004-opentelemetry.md), [docs/telemetry.md](docs/telemetry.md)):
  - Log only through `telemetry.Logger("<pkg path>")` (slog). No `fmt.Print`,
    `log.Print` or other logging libraries, except the stderr fallback when
    telemetry setup itself fails.
  - Report errors with `telemetry.Fail(ctx, span, log, msg, err, attrs...)`,
    which records on the span and logs with the same attributes.
  - Console telemetry never writes to **stdout**. The CLI uses it for
    NDJSON output, and the stdio scaffold adapters use it for the protocol
    until M4. Local-process adapters (ADR 9 A13) log through telemetry
    that the core captures and forwards; they get no exporter settings of
    their own.
  - New spans or metrics go into the catalog in `docs/telemetry.md` in the
    same change.
  - Outbound HTTP uses `otelhttp` so upstream calls appear in the trace.
- **Secrets** come from environment variables named in config (for example
  `TokenEnv`, `BEARING_STORE_PASSWORD`), never from config values, URLs or
  logs.
- Spec conventions: RFC 2119 keywords, `snake_case` JSON (CloudEvents
  envelope fields excepted), RFC 3339 UTC times.

## Style guide

Codifies what the existing code does and sets rules for patterns it doesn't
have yet (`NewID`, `internal/testkit`, golden files). Code that predates a
rule is not a finding unless the PR changes it. golangci-lint (`make lint`)
enforces formatting and mechanical naming and error rules (initialisms,
stutter, lower-case error strings, `errors.Is`, `ctx` first); this list is
what it doesn't catch.
`go-reviewer` checks it.

**Naming**

- Name packages for what they provide (`memstore`, `telemetry`), never
  `util` or `common`.
- Typed strings for domain values: `model.Kind`, `model.Key`,
  `contracts.SubjectID`, with `Kind…`/`Rel…` constants.
- Assert interface satisfaction at compile time:
  `var _ contracts.GraphStore = (*Store)(nil)`.

**Errors**

- Errors returned from a package's entry points (`Open`, `New`, `Setup`)
  start with the package name: `fmt.Errorf("store: parse URL: %w", err)`.
  Methods behind a contract and inner helpers add only the action and
  subject (`"decode facts: %w"`, `"entity %s: %w"`), because the caller's
  span names the component.
- Sentinels are package-level `ErrX` values built with `errors.New`
  (`contracts.ErrNotFound`, `adapter.ErrTooManyPages`).
  `contracts.ErrNotFound` means "no match".
- Wrap sentinels with the subject so the message says what was missing:
  `fmt.Errorf("entity %s: %w", id, contracts.ErrNotFound)`.
- Don't log and return the same error; return it, and let the caller with
  the span report it via `telemetry.Fail`.
- Error text never contains secrets (see `redact` in `pkg/store`).

**Doc comments**

- Every package has a `// Package x …` comment (`// Command x …` for
  `main`) that says its role and where it fits.
- Every exported identifier has a doc comment, including methods that
  implement an interface (revive enforces it). For those one line is
  enough: `// Apply implements contracts.GraphStore.`
- Short. Say why, a constraint, or a non-obvious default, not what the code
  already says: `// Getenv reads BEARING_STORE_PASSWORD; nil means os.Getenv.`
  Match the surrounding comment density.

**Functions and interfaces**

- Never store a `context.Context` in a struct.
- Keep interfaces small and define them where they are used, as
  `surrealstore.Querier` is. `pkg/contracts` is the deliberate exception:
  component boundaries live there.
- Configuration is a struct with defaults applied in one place
  (`parseConfig`, `store.Config`), not functional options.

**Dependency injection**

- Inject time, environment, HTTP and ID generation as fields: `Now func()
  time.Time`, `Getenv func(string) string`, `HTTP *http.Client` (see
  `adapters/github`), `NewID func() string`. Constructors fill real
  defaults (`time.Now`, `os.Getenv`, an `otelhttp` client); tests set fakes.
  Code that needs timers takes a `clock.Clock` from `pkg/clock` (`Now`,
  `After`, `NewTimer`, `NewTicker`); `testkit.FakeClock` implements it.
- Code under test never calls `time.Now`, `os.Getenv`, `rand` or the
  network directly. Measuring a duration for a metric is the exception.

**Tests**

- Name tests `Test<Subject><Behavior>` as a sentence:
  `TestSyncAllStopsRunawayAdapters`, `TestHandleRejectsBadSignature`.
- Table-driven when cases share a shape (`TestKeyParse`), with `t.Run` named
  by the case.
- Failure messages say got and want: `t.Fatalf("got %v, want %v", got, want)`.
- Tests use `httptest` servers, never the real network.
- Fakes over mocks: use `internal/testkit` fakes first; production code
  never imports testkit. Fakes only one package needs (`httptest` handlers,
  small structs like `pager`) stay in its test files. No mock frameworks.
- Assert on behavior the code under test produced. A test that only checks
  what a fake was told to return proves nothing.
- Golden files live under the package's `testdata/` and are rewritten with
  `go test ./pkg/x -update` (a package-level `var update = flag.Bool("update",
  false, …)`); review golden diffs like code.
- Backends prove themselves with `pkg/contracts/conformance`, not their own
  ad hoc copies of it.
- Tests needing an external service skip unless an env var
  (`BEARING_TEST_<NAME>`) is set; `make test` stays hermetic.

## Changing the design

- **Data model** (kinds, relations, observation fields): update
  `docs/spec/data-model.md`, `proto/bearing/model` (then `make generate`),
  the registry and validation in `pkg/model` and, if useful,
  `testdata/observations/` together. Never hand-write a schema or codec;
  everything is generated from `proto/`. Adding is compatible; removing or
  changing meaning needs a new package version (see `docs/spec/README.md`).
- **Adapter protocol:** update `docs/spec/adapter-protocol.md` and
  `pkg/adapter` together; bump `adapter.ProtocolVersion` (reported in
  `bearing.describe`) for breaking changes.
- **Contracts:** update `docs/spec/contracts.md`, `pkg/contracts`, the
  conformance suite, the instrument wrapper, and every backend
  (`memstore`, `surrealstore`) together.
- **ADR diagrams** are SVGs generated from `docs/adr/diagrams/src/`; edit
  the Python there and re-export (see `docs/adr/diagrams/README.md`), never
  the SVGs by hand.
- **Significant decisions** get an ADR: copy `docs/adr/template.md` to the
  next number, and mark older ADRs "Superseded in part by" when relevant.
- Keep `README.md`'s "What's here" table current when adding top-level
  packages or docs.

## Recipes

- **New adapter:** see [`.claude/skills/new-adapter/SKILL.md`](.claude/skills/new-adapter/SKILL.md).
- **New contract backend:** see [`.claude/skills/new-backend/SKILL.md`](.claude/skills/new-backend/SKILL.md).

Both are plain Markdown checklists; any agent can follow them.

## Commits and PRs

- Commit messages: imperative subject line under about 72 characters
  ("Add PagerDuty adapter"), then a body saying why and anything reviewers
  should know (untested paths, new dependencies, spec changes).
- Keep changes focused. Spec, code and tests for one change land together.
- PR descriptions follow [`.github/pull_request_template.md`](.github/pull_request_template.md).
- CI (`.github/workflows/ci.yml`) runs everything `make check` does, as jobs
  that run side by side so a push gets its answer in minutes: `static`
  (`make static`: everything but the tests), `test` (`make cover`, with
  SurrealDB), `baseline` (the same tests at the merge base, `make
  covergate-base`) and `check` (`make covergate-report`, which compares the
  two profiles and fails unless the other jobs passed). Keep the whole run
  to about five minutes: a test that takes longer than a minute or two on
  its own gets split up or made parallel (`t.Parallel` with a store per
  test), and nothing slow should be added to `static`.

## Review and merge process

- **Reviewers.** Each PR gets one or two reviewers, chosen from
  [`.github/reviewers.yml`](.github/reviewers.yml) by the paths it touches.
  If `security` matches, it always takes one of the two slots. If more than
  two domain reviewers match, the lead picks the two most relevant and says
  why in the PR. `go` takes the second slot when only one domain reviewer
  matches and the PR changes Go, and is the only reviewer when no domain
  reviewer matches. A PR that matches no rule gets the file's `default`.
  Reviewer definitions live in [`.claude/agents/`](.claude/agents/).
- **Findings.** Every finding states severity, effort to fix now, how hard
  it is to change later, and whether leaving it opens an important gap
  against the project's goals. A defect that makes the change wrong,
  unsafe, or not do what it claims (a bug, a missing check, a test that
  cannot fail) leaves an important gap. Style and polish do not. Only
  findings that are hard to change later or leave an important gap block;
  the rest become follow-up issues. The lead files them as GitHub issues
  linked from the PR before merging.
- **Rounds.** A round is one push answering the findings plus the
  reviewers' re-review. Up to five rounds per PR. After the fifth round
  with blocking findings open, the lead stops and mentions @dhm116 (Doug,
  the maintainer) in that review's summary comment. Rounds and reviews are
  numbered differently: the first review comes before any round, so round
  k's re-review is Review k+1 and the fifth round ends with Review 6.
- **Review summary.** After every review, including the first and one
  that approves outright, the lead posts one PR comment headed "Review N"
  (the first review is 1). It gives each reviewer's verdict, then each
  finding with its status: blocking ones open, fixed in a named commit, or
  withdrawn with the reason; non-blocking ones fixed in a named commit,
  filed as a follow-up issue, or open until that issue is filed. When a
  later push settles an open item, the lead edits that comment. An edit
  sends no notification, so the next "Review N" comment also lists the
  earlier items it settled, for example "Settles from Review 1: ...".
  Someone reading only the PR sees what the reviewers caught and how each
  item was settled.
- **Merge.** The lead merges once CI passes (or the change needs no tests),
  the assigned reviewers approve, and every review summary shows each
  finding as fixed, withdrawn or linked to an issue.
- **Milestones** close only with Doug's review and approval.
- **Shared files** are owned by the lead: `proto/`, `gen/go`,
  `pkg/contracts`, `docs/spec/contracts.md`, `go.mod`/`go.sum`, `Makefile`,
  `AGENTS.md`, `docs/telemetry.md`. Other contributors propose changes to
  them through the lead. Contract changes (`pkg/contracts`, `proto/`,
  `docs/spec/contracts.md`) land first, in their own PR, before code that
  depends on them.

## Brand

Logos, colors and type live in [`docs/brand/`](docs/brand/). Use the SVGs
there rather than redrawing the mark.
