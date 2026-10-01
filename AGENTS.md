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
| `make lint` | Fails if `gofmt -l .` lists anything, then `go vet ./...` |
| `make test` | `go vet ./...` and `go test ./...` (no external services needed) |
| `make build` | Builds `bin/bearing` and `bin/bearing-adapter-github` |
| `make fmt` | `gofmt -w .` |
| `make test-surrealdb SURREALDB=ws://127.0.0.1:8000` | SurrealDB conformance suites against a running server |
| `make test-embedded SURREALDB_LIB=<dir>` | Same suites against embedded SurrealDB (CGO, needs `libsurrealdb_c.a`) |

Run `make lint test` before every commit. A single package:
`go test ./pkg/model/ -run TestSchema`.

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
| `schema/observation.v1.schema.json` | JSON Schema for observations; must list every kind and relation in `pkg/model` (a test checks). |
| `testdata/observations.ndjson` | Example observations; decoded and validated by tests. |
| `pkg/model` | Entity kinds, relation types, keys, the observation envelope and its validation. |
| `pkg/adapter` | Adapter protocol: `Adapter` interface, `ServeStdio` for adapter authors, client for the core. |
| `pkg/contracts` | Interfaces between components (`GraphStore`, `VectorIndex`, `EventBus`, `Judge`, …). |
| `pkg/contracts/conformance` | Test suites every backend must pass. |
| `pkg/contracts/instrument` | OpenTelemetry wrappers so every backend gets the same spans and metrics. |
| `pkg/store` | Opens graph store and vector index from URLs (`mem://`, `surrealdb+ws://`, `surrealkv://`, …). |
| `pkg/telemetry` | OpenTelemetry setup, `Logger`, `Tracer`, `Meter`, `Fail`. |
| `internal/memstore` | In-memory reference backend for both contracts. |
| `internal/surrealstore` | SurrealDB backend (server mode pure Go; embedded mode behind `surrealembed`). |
| `adapters/github` | GitHub adapter, the worked example for new adapters. |
| `cmd/bearing` | Developer CLI: `adapter describe`, `adapter sync`, `validate`. |
| `cmd/bearing-adapter-github` | Binary that serves the GitHub adapter on stdio. |

The Go module path is the placeholder `bearing.example`. Import packages as
`bearing.example/pkg/...`; don't rename the module unless asked.

## Architecture rules

These come from the ADRs and project decisions. Don't break them without a
new ADR.

1. **The graph is the source of truth** ([ADR 2](docs/adr/0002-graph-is-source-of-truth.md)).
   Every vector points at a graph entity and the index can be rebuilt from
   the graph. Ownership and policy answers read only asserted facts; results
   from semantic search are verified in the graph.
2. **Adapters are separate processes** speaking JSON-RPC 2.0 over stdio, one
   message per line ([ADR 3](docs/adr/0003-adapter-protocol.md)). Methods:
   `bearing.describe`, `bearing.sync` (cursor paged), optional
   `bearing.handle` for webhooks. Adapters are stateless, read-only against
   their source, verify webhook signatures, and never resolve identities
   across systems. That is the core's job.
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
   must be Apache-2.0 compatible.

## Code conventions

- **Telemetry** ([ADR 4](docs/adr/0004-opentelemetry.md), [docs/telemetry.md](docs/telemetry.md)):
  - Log only through `telemetry.Logger("<pkg path>")` (slog). No `fmt.Print`,
    `log.Print` or other logging libraries, except the stderr fallback when
    telemetry setup itself fails.
  - Report errors with `telemetry.Fail(ctx, span, log, msg, err, attrs...)`,
    which records on the span and logs with the same attributes.
  - Console telemetry never writes to **stdout**. Adapters use stdout for the
    protocol and the CLI uses it for NDJSON output.
  - New spans or metrics go into the catalog in `docs/telemetry.md` in the
    same change.
  - Outbound HTTP uses `otelhttp` so upstream calls appear in the trace.
- **Testability:** time, environment and HTTP are injected (`Now`,
  `Getenv`, `HTTP` fields; see `adapters/github`). Tests use `httptest`
  servers, never the real network, and table-driven cases where they help.
- **Secrets** come from environment variables named in config (for example
  `TokenEnv`, `BEARING_STORE_PASSWORD`), never from config values, URLs or
  logs.
- Errors are wrapped with the package name as prefix, e.g.
  `fmt.Errorf("surrealstore: open embedded %s: %w", endpoint, err)`.
  `contracts.ErrNotFound` means "no match".
- Every package has a package doc comment explaining its role; exported
  identifiers have doc comments. Match the surrounding comment density.
- Spec conventions: RFC 2119 keywords, `snake_case` JSON (CloudEvents
  envelope fields excepted), RFC 3339 UTC times.

## Changing the design

- **Data model** (kinds, relations, observation fields): update
  `docs/spec/data-model.md`, `pkg/model/model.go`,
  `schema/observation.v1.schema.json` and, if useful,
  `testdata/observations.ndjson` together. Adding is compatible; removing or
  changing meaning needs `v2` (see `docs/spec/README.md`).
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
- CI (`.github/workflows/ci.yml`) runs `make lint test build`.

## Brand

Logos, colors and type live in [`docs/brand/`](docs/brand/). Use the SVGs
there rather than redrawing the mark.
