<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/bearing-mark-dark.svg">
    <img src="docs/brand/bearing-mark-light.svg" alt="Bearing logo: a lowercase b whose stem ends in a north arrow" width="96">
  </picture>
</p>

<h1 align="center">Bearing</h1>

<p align="center"><em>What is this, who owns it, what changed, and how do I safely change it?<br>One live answer for every engineer and every agent.</em></p>

Bearing is an open-source foundation for a developer platform. It builds a
live map of an engineering organization (services, owners, dependencies,
changes, incidents) from the tools the organization already uses, and serves
it to engineers and AI agents wherever they work.

Bearing doesn't replace existing tools. It defines the data worth having and
keeps the adapters that fetch it small.

> Status: restarting from a smaller scope (Go, DuckDB, gRPC adapters). See
> the [roadmap](ROADMAP.md). Parts of this README still describe the code
> from before the restart (tag `pre-pivot`) and are rewritten as that code
> is replaced.

## What's here

| Path | What it is |
| --- | --- |
| [`AGENTS.md`](AGENTS.md) | Guide for AI coding agents and new contributors: commands, layout, rules |
| [`ROADMAP.md`](ROADMAP.md) | What the MVP builds now, and what waits |
| [`docs/spec/`](docs/spec/) | The specification: the server API, and the data model (being rewritten) |
| [`docs/adr/`](docs/adr/) | Architecture decision records |
| [`docs/brand/`](docs/brand/) | Logo, colors, type and the diagram-design profile |
| [`docs/telemetry.md`](docs/telemetry.md) | Telemetry configuration, spans and metrics |
| [`docs/review-process.md`](docs/review-process.md) | How pull requests are reviewed and merged |
| [`SECURITY.md`](SECURITY.md) | How to report a vulnerability privately |
| [`proto/`](proto/) | Protobuf sources (`bearing.model.v1alpha1`, `bearing.event.v1alpha1`, `bearing.config.v1alpha1`, `bearing.resolver.v1alpha1`): the only source of truth for the data model, events and configuration |
| [`gen/`](gen/) | Code generated from `proto/` by `make generate`: Go types in `gen/go`, JSON Schema in `gen/jsonschema` |
| [`pkg/model`](pkg/model) | Helpers around the generated types: kind and predicate registry, keys, validation, `fact_id` |
| [`pkg/adapter`](pkg/adapter) | The adapter protocol: server helper for adapter authors, client for the core |
| [`pkg/contracts`](pkg/contracts) | Interfaces between components (graph store, event log) |
| [`pkg/resolver`](pkg/resolver) | Turns validated observations into a ChangeSet: identity, claims, supports and status |
| [`pkg/server`](pkg/server) | The server's parts: webhook ingest that checks each delivery's signature and logs it before answering, and the workers that apply the log, the scheduler, the health checks and the API for callers |
| [`pkg/server/boot`](pkg/server/boot) | Builds and runs `bearing server` from its settings file |
| [`pkg/apiclient`](pkg/apiclient) | The client of the server's API: asks a running server the questions `pkg/query` answers over a store, and requests a sync |
| [`pkg/query`](pkg/query) | Answers questions from a graph store: get, owner, related and changes, each with its sources, events, confidence and observed times |
| [`pkg/contracts/instrument`](pkg/contracts/instrument) | OpenTelemetry wrappers that give every backend the same spans and metrics |
| [`pkg/telemetry`](pkg/telemetry) | OpenTelemetry setup: logs, traces, metrics and exporters |
| [`pkg/contracts/conformance`](pkg/contracts/conformance) | Test suites every backend must pass |
| [`pkg/store`](pkg/store) | Opens the graph store and event log from a URL (`mem://`) |
| [`pkg/clock`](pkg/clock) | Clock interface for time, timers and tickers, so tests can drive time |
| [`internal/memstore`](internal/memstore) | In-memory graph store and event log, until the DuckDB store replaces it |
| [`internal/testkit`](internal/testkit) | Test fakes: clock, deterministic IDs, scripted and recorded HTTP servers, secret canaries and leak scanning |
| [`internal/fakes`](internal/fakes) | Fake GitHub (REST and GraphQL), Authentik-like directory and Kubernetes API servers for one fictional company, with scripted timelines; their recorded feeds are in [`testdata/acme`](testdata/acme) |
| [`adapters/github`](adapters/github) | The GitHub adapter |
| [`cmd/bearing`](cmd/bearing) | Developer CLI: runs and checks adapters, answers `get`, `owner`, `related` and `changes` from the store or from a running server, asks a server to sync a source, and runs the server (`bearing server --settings`) |
| [`site/`](site/) | The project website: a small Go generator that renders an introduction, the roadmap and the spec for GitHub Pages |
| [`tools`](tools) | Pinned developer tools (golangci-lint, govulncheck, buf and its plugins) and the coverage gate, in their own Go module |

## Try it

Requires Go 1.27.2 or later (the `go` command downloads it automatically if needed).

```sh
make check         # everything CI runs: generated code is current, lint, tests with the coverage gate, govulncheck, build
make generate      # after editing proto/: format, lint and regenerate gen/
make test          # vet and run every test
make build         # builds bin/bearing and bin/bearing-adapter-github

# What does the GitHub adapter emit and need?
bin/bearing adapter describe -- bin/bearing-adapter-github

# Sync an organization; observations are checked as they arrive
export GITHUB_TOKEN=...   # read-only: metadata, contents, members
echo '{"org":"your-org"}' > github.json
bin/bearing adapter sync --config github.json -- bin/bearing-adapter-github > obs.ndjson
```

## Design in one paragraph

Adapters read a source system and emit **observations**: CloudEvents that say
"this entity exists, with these attributes and these relations", plus where
the information came from. The core resolves keys from different systems to
one entity, scores each relation's confidence, and stores the result as
**facts**, each with the observations behind it. See the
[roadmap](ROADMAP.md) for where this is going.

## Storage

The store is moving to DuckDB ([ADR 5](docs/adr/0005-duckdb-store.md)). Until
that lands, `mem://` (in memory, for tests and demos) is the only store.

## Telemetry

Every binary emits OpenTelemetry logs, traces and metrics, configured with
the standard `OTEL_*` variables. By default logs go to stderr and traces and
metrics are off; set `OTEL_EXPORTER_OTLP_ENDPOINT` to send everything to a
collector. One sync is one trace across the CLI, the adapter process and
every upstream API call. See [docs/telemetry.md](docs/telemetry.md) for the
span and metric catalog.

## First adapters

- **GitHub**: repositories, CODEOWNERS, teams, members (this repo)
- **AWS**: accounts, tagged resources, CloudTrail changes (planned)
- **PagerDuty**: services, schedules, escalation policies, incidents (planned)

## Module path

The Go module path `bearing.example` is a placeholder until the project has
a home. Changing it later is a single find-and-replace.

## Brand

The Bearing mark is a lowercase b whose stem ends in a north arrow and whose
bowl is a compass ring. The accent is a deep sea teal (`#1D5E74` on light
backgrounds, `#7FC0D6` on dark), and the type is Bricolage Grotesque for
display, IBM Plex Sans for text and IBM Plex Mono for code. The logo files,
full palette, type scale and usage rules are in [docs/brand](docs/brand/).

## License

Apache License 2.0. See [LICENSE](LICENSE).
