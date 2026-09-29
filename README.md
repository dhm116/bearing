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

> Status: early scaffold. The schema, adapter protocol and component
> contracts are drafts at version 0.1 and will change.

## What's here

| Path | What it is |
| --- | --- |
| [`AGENTS.md`](AGENTS.md) | Guide for AI coding agents and new contributors: commands, layout, rules |
| [`docs/spec/`](docs/spec/) | The specification: data model, adapter protocol, component contracts |
| [`docs/adr/`](docs/adr/) | Architecture decision records |
| [`docs/brand/`](docs/brand/) | Logo, colors and type |
| [`docs/telemetry.md`](docs/telemetry.md) | Telemetry configuration, spans and metrics |
| [`schema/observation.v1.schema.json`](schema/observation.v1.schema.json) | JSON Schema for the observation envelope |
| [`pkg/model`](pkg/model) | Go types for entity kinds, relations and observations |
| [`pkg/adapter`](pkg/adapter) | The adapter protocol: server helper for adapter authors, client for the core |
| [`pkg/contracts`](pkg/contracts) | Interfaces between components (graph store, vector index, judge, policy, executor, …) |
| [`pkg/contracts/instrument`](pkg/contracts/instrument) | OpenTelemetry wrappers that give every backend the same spans and metrics |
| [`pkg/telemetry`](pkg/telemetry) | OpenTelemetry setup: logs, traces, metrics and exporters |
| [`pkg/contracts/conformance`](pkg/contracts/conformance) | Test suites every backend must pass |
| [`pkg/store`](pkg/store) | Opens the graph store and vector index from URLs (`mem://`, `surrealdb+ws://`, …) |
| [`internal/memstore`](internal/memstore) | In-memory graph store and vector index, the reference implementation |
| [`internal/surrealstore`](internal/surrealstore) | SurrealDB backend for both the graph and vectors (server or embedded) |
| [`adapters/github`](adapters/github) | The GitHub adapter |
| [`cmd/bearing`](cmd/bearing) | Developer CLI for running and checking adapters |

## Try it

Requires Go 1.27.1 or later (the `go` command downloads it automatically if needed).

```sh
make test          # vet and run every test
make test-surrealdb SURREALDB=ws://127.0.0.1:8000   # also run the SurrealDB conformance suites
make build         # builds bin/bearing and bin/bearing-adapter-github

# What does the GitHub adapter emit and need?
bin/bearing adapter describe -- bin/bearing-adapter-github

# Sync an organization and validate the output
export GITHUB_TOKEN=...   # read-only: metadata, contents, members
echo '{"org":"your-org"}' > github.json
bin/bearing adapter sync --config github.json -- bin/bearing-adapter-github > obs.ndjson
bin/bearing validate obs.ndjson
```

## Design in one paragraph

Adapters read a source system and emit **observations**: CloudEvents that say
"this entity exists, with these attributes and these relations", plus where
the information came from. The core resolves keys from different systems to
one entity, scores each relation's confidence, and stores the result as
**facts** in a graph that is the source of truth. A semantic index sits beside
the graph for fuzzy search. Every component talks through an interface in
[`pkg/contracts`](pkg/contracts) with one default backend, and any other
backend that passes the conformance suite can replace it.

## One database to start

The graph and the semantic index are separate contracts, but by default one
SurrealDB database serves both, so there is one thing to run, or nothing.
[`pkg/store`](pkg/store) opens a store from a URL:

| Store URL | What runs |
| --- | --- |
| `mem://` | Nothing; in-memory, for tests and demos |
| `surrealdb+ws://root@localhost:8000` | One `surreal start` process |
| `surrealkv:///var/lib/bearing` | SurrealDB inside Bearing (`go build -tags surrealembed`, needs CGO) |

Larger installs can move vectors to a dedicated engine later by giving the
vector index its own URL. See [ADR 5](docs/adr/0005-one-store-to-start.md).

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
