# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository. Read
this first, then the [roadmap](ROADMAP.md) for what is being built now.

## What Bearing is

Bearing answers questions about an engineering organization from the tools
it already uses. Small **adapters** read a source (GitHub, an identity
provider, Kubernetes) and emit **observations**. The core resolves them
into **entities** and **facts** with their provenance, stores them in
DuckDB, and answers queries over a CLI, a REST API and an MCP endpoint.

The MVP is done when it answers these, with the evidence behind each answer:

1. Which team owns service X?
2. Which services use language X below version Y?
3. What changed between times X and Y?
4. What does service X depend on, and how?
5. What depends on service X, and how?

## The pivot

On 2026-10-20 Bearing restarted from a smaller scope. Everything before is
tagged [`pre-pivot`](https://github.com/dhm116/bearing/tree/pre-pivot).

- The ADRs in [`docs/adr/`](docs/adr/) are the only design rules. The
  pre-pivot ADRs are history: read them at the tag for ideas, never cite
  them as requirements.
- Some pre-pivot code is still in the tree until the roadmap replaces it:
  `internal/memstore`, `pkg/contracts` and its conformance suite (step 2),
  and the stdio adapter transport in `pkg/adapter` (step 3).
  `docs/spec/data-model.md` and comments that cite removed docs describe
  that code, not the target. Don't extend them, and don't flag new code for
  disagreeing with them.
- Reuse what serves the MVP (`pkg/model`, `pkg/telemetry`, `pkg/clock`,
  `internal/testkit`, `internal/fakes` and the Acme fixtures,
  `adapters/github`, `pkg/query`). Delete the rest rather than keeping it
  beside the new code.

## Commands

Go 1.27.2 or later; the `go` command downloads the toolchain if needed. A C
compiler is required (DuckDB needs CGO).

| Command | What it does |
| --- | --- |
| `make check` | Everything CI runs |
| `make test` | `go vet ./...` and `go test ./...`, no external services |
| `make lint` | golangci-lint (pinned in `tools/go.mod`, config in `.golangci.yml`) |
| `make fmt` | gofumpt and goimports |
| `make generate` | After editing `proto/`: buf format, lint and generate into `gen/`. Commit the output |
| `make build` | Builds the binaries into `bin/` |

Run `make check` before every commit. One package:
`go test ./pkg/model/ -run TestInvalidObservations`.

Add dependencies with `go get <module>@<version>`, with the reason in the
commit message.

## Architecture rules

From the ADRs. Don't break them without a new ADR.

1. **Go, Apache-2.0, few dependencies; CGO allowed**
   ([ADR 1](docs/adr/0001-go-apache-few-dependencies.md)).
2. **Facts carry their provenance**
   ([ADR 2](docs/adr/0002-facts-carry-provenance.md)). Every fact names the
   observations behind it. Deciding answers read only asserted facts;
   inferred links are stored as proposals with evidence. No audit log.
3. **OpenTelemetry everywhere** ([ADR 3](docs/adr/0003-opentelemetry.md)).
4. **Protobuf data model; adapters speak gRPC**
   ([ADR 4](docs/adr/0004-protobuf-and-grpc-adapters.md)). The same service
   locally and remotely. Adapters are stateless, read-only against their
   source, and never resolve identities across systems.
5. **DuckDB is the store** ([ADR 5](docs/adr/0005-duckdb-store.md)). One
   implementation, `:memory:` in tests. Batch writes. No full-text or vector
   search yet.
6. **Replace nothing; integrate.** Adapt existing tools (Backstage included)
   rather than competing with them.

## Code conventions

- **Telemetry:** log only through `telemetry.Logger("<pkg path>")`; report
  errors with `telemetry.Fail`; never write telemetry to stdout; outbound
  HTTP uses `otelhttp`; list new spans and metrics in `docs/telemetry.md`.
- **Secrets** come from environment variables named in config, never from
  config values, URLs, logs or error text.
- JSON is `snake_case` (CloudEvents envelope fields excepted); times are RFC
  3339 UTC.

## Style guide

golangci-lint enforces formatting and mechanical rules; this is what it
doesn't catch. `go-reviewer` checks it. Code that predates a rule is not a
finding unless the PR changes it.

**Naming.** Packages are named for what they provide, never `util` or
`common`. Domain values are typed strings (`model.Kind`, `model.Key`).
Assert interface satisfaction at compile time:
`var _ Store = (*DuckStore)(nil)`.

**Errors.**

- Errors from a package's entry points (`Open`, `New`) start with the
  package name: `fmt.Errorf("store: open: %w", err)`. Inner helpers add only
  the action and subject: `"entity %s: %w"`.
- Sentinels are package-level `ErrX` values; wrap them with the subject.
- Don't log and return the same error; the caller with the span reports it.

**Doc comments.** Every package has a `// Package x …` comment and every
exported identifier a doc comment. Short: say why or a constraint, not what
the code says.

**Functions and interfaces.** Never store a `context.Context` in a struct.
Keep interfaces small and define them where they are used. Configuration is
a struct with defaults applied in one place, not functional options.

**Dependency injection.** Inject time, environment, HTTP and IDs as fields
(`Now func() time.Time`, `Getenv`, `HTTP *http.Client`, `NewID`), or a
`clock.Clock` for timers. Code under test never calls `time.Now`,
`os.Getenv`, `rand` or the network directly.

## Tests

Velocity is bounded by how long tests take, so they are **fast**, focused
and consistent.

- Target: `make test` under a minute. A test that takes more than a second or
  two on its own is split up, made parallel (`t.Parallel`) or justified in
  the PR.
- External systems are faked, never called: `httptest` servers,
  `internal/fakes` (GitHub, directory, Kubernetes for the Acme org),
  `internal/testkit` (fake clock, IDs, secrets). The store is real: DuckDB
  `:memory:`.
- Name tests `Test<Subject><Behavior>` as a sentence. Table-driven when
  cases share a shape, with `t.Run` named by the case. Failures say got and
  want.
- Assert on behavior the code produced. A test that only checks what a fake
  was told to return proves nothing.
- Golden files live under the package's `testdata/` and are rewritten with
  `go test ./pkg/x -update`.
- Each MVP question has an end-to-end test on the Acme scenario with a
  golden answer.

## How work is done

- **Plan, then fan out.** An Opus lead writes a detailed plan for each work
  stream: files, interfaces, tests and acceptance checks. Up to three
  streams run at once, each in its own worktree under `.worktrees/`
  (git-ignored): `git worktree add .worktrees/<name> -b <branch>`. Shared
  foundations (the data model, the store interface) land first, alone,
  before streams that depend on them start.
- **Pick the model by the work**, when a persona doesn't say:
  - **Opus:** rulings, security or auth reviews, design forks with real
    trade-offs, subtle concurrency or data-correctness bugs, long
    multi-step work where a wrong early call is costly to unwind.
  - **Sonnet:** implementation against an approved plan, code review,
    routine refactors and tests, careful bulk reading or summarizing.
  - **Haiku:** quick lookups, small edits, mechanical batch work, fan-out
    where many cheap calls beat one careful one.
- **80–90% first.** Build the common case. When an edge case would cost
  more than it's worth now, record it in
  [`docs/edge-cases.md`](docs/edge-cases.md) (context, a concrete example,
  a proposed fix) and move on.
- **Docs last.** Write and update docs as one of the final steps before
  the PR, once behavior has settled. Audience: a junior engineer. Short,
  plain, with a diagram where it helps (Mermaid in Markdown, or SVG).

## Changing the design

- **Data model or adapter service:** edit `proto/`, run `make generate`,
  update `pkg/model` validation and `testdata/` in the same change.
- **Significant decisions** get an ADR: copy
  [`docs/adr/template.md`](docs/adr/template.md) to the next number. Keep it
  to about a page.
- **Deferred ideas** go in the [roadmap](ROADMAP.md), not in code. No
  placeholder packages or interfaces for work that isn't scheduled.

## Commits and PRs

- Commit messages: imperative subject under about 72 characters, then a
  body saying why and anything reviewers should know.
- Keep changes focused. Code, tests and docs for one change land together.
- `main` holds only what Bearing ships or needs to build, test and document
  itself. Spikes live on `spike/<name>` branches and are never merged.
- Go, plus POSIX shell for short glue. No other language for tooling.
- PR descriptions follow
  [`.github/pull_request_template.md`](.github/pull_request_template.md).

## Review and merge process

See [`docs/review-process.md`](docs/review-process.md): up to three expert
reviewers in parallel, the ponytail reviewer weighing in on each, one
summary, and the review-round card posted with `tools/reviewcard`.

## Brand

Logos, colors and type live in [`docs/brand/`](docs/brand/).
