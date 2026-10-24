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
| [`docs/spec/`](docs/spec/) | The specification: concepts and the server API |
| [`docs/architecture.md`](docs/architecture.md) | A tour of the running server: adapters, event log, workers, store and matcher |
| [`docs/diagrams/`](docs/diagrams/) | Source and exported SVGs of the diagrams (`combine.sh` makes them follow dark mode) |
| [`docs/edge-cases.md`](docs/edge-cases.md) | Cases left for later, each with an example and a proposed fix |
| [`docs/identity-evaluation.md`](docs/identity-evaluation.md) | How well the matcher decides that two records are one thing |
| [`docs/security/threat-model.md`](docs/security/threat-model.md) | Trust boundaries, controls and known gaps |
| [`docs/adr/`](docs/adr/) | Architecture decision records |
| [`docs/brand/`](docs/brand/) | Logo, colors, type and the diagram-design profile |
| [`docs/mcp.md`](docs/mcp.md) | Adding `bearing mcp` to Claude Code or Claude Desktop, and its tools |
| [`docs/telemetry.md`](docs/telemetry.md) | Telemetry configuration, spans and metrics |
| [`docs/review-process.md`](docs/review-process.md) | How pull requests are reviewed and merged |
| [`SECURITY.md`](SECURITY.md) | How to report a vulnerability privately |
| [`proto/`](proto/) | Protobuf sources (`bearing.model.v1alpha1`, `bearing.adapter.v1alpha1`, `bearing.event.v1alpha1`, `bearing.config.v1alpha1`): the only source of truth for the data model, the adapter service, events and configuration |
| [`gen/`](gen/) | Code generated from `proto/` by `make generate`: Go types in `gen/go`, JSON Schema in `gen/jsonschema` |
| [`pkg/model`](pkg/model) | Helpers around the generated types: kind and predicate registry, keys, validation, `fact_id` |
| [`pkg/adapter`](pkg/adapter) | The adapter service over gRPC: `Main` and `Serve` for adapter authors, `Start` and `Dial` for the core |
| [`pkg/store`](pkg/store) | The store module: the `Store` interface (graph, event log, ping, close), the event log interface, opening a store from a URL through registered drivers (`mem://`, `duckdb:///path`), and telemetry for every store |
| [`pkg/store/storetest`](pkg/store/storetest) | The suite every store implementation must pass |
| [`pkg/graph`](pkg/graph) | The graph capabilities a store provides: ingest, entity, find, facts, walk and changes |
| [`pkg/server`](pkg/server) | The server's parts: webhook ingest that checks each delivery's signature and logs it before answering, and the workers that apply the log, the scheduler, the health checks and the API for callers |
| [`pkg/server/boot`](pkg/server/boot) | Builds and runs `bearing server` from its settings file |
| [`pkg/apiclient`](pkg/apiclient) | The client of the server's API: asks a running server the questions `pkg/query` answers, and requests a sync |
| [`pkg/query`](pkg/query) | Answers questions from the store: get, owner, related, changes and find, each fact with its sources |
| [`pkg/match`](pkg/match) | The matcher: finds records that may describe one thing and writes `same_as` links, asserted by key rules and proposed with evidence otherwise |
| [`pkg/telemetry`](pkg/telemetry) | OpenTelemetry setup: logs, traces, metrics and exporters |
| [`pkg/clock`](pkg/clock) | Clock interface for time, timers and tickers, so tests can drive time |
| [`internal/duckstore`](internal/duckstore) | The DuckDB store: observations, entities, facts and the event log; registers `mem://` and `duckdb://` |
| [`internal/e2e`](internal/e2e) | End-to-end tests on the fictional Acme org |
| [`internal/identityeval`](internal/identityeval) | A labelled set of record pairs from the Acme org, and tests that score the matcher's precision and recall on it |
| [`internal/testkit`](internal/testkit) | Test fakes: clock, deterministic IDs, scripted and recorded HTTP servers, secret canaries and leak scanning |
| [`internal/fakes`](internal/fakes) | Fake GitHub (REST and GraphQL), Authentik-like directory and Kubernetes API servers for one fictional company, with scripted timelines; their recorded feeds are in [`testdata/acme`](testdata/acme) |
| [`adapters/github`](adapters/github) | The GitHub adapter: repositories, teams, members, CODEOWNERS and language versions from manifests |
| [`adapters/kubernetes`](adapters/kubernetes) | The Kubernetes adapter: workloads, namespaces, team labels, source repositories and dependencies between workloads |
| [`adapters/authentik`](adapters/authentik) | The Authentik directory adapter: users, groups, nested groups, time-bounded memberships and GitHub account links |
| [`cmd/bearing`](cmd/bearing) | Developer CLI: runs and checks adapters, answers `get`, `owner`, `related`, `changes`, `find` and `runtimes` from the store or from a running server, asks a server to sync a source, serves the queries to agents as an MCP server (`bearing mcp`), and runs the server (`bearing server --settings`) |
| [`cmd/bearing-adapter-github`](cmd/bearing-adapter-github), [`cmd/bearing-adapter-kubernetes`](cmd/bearing-adapter-kubernetes), [`cmd/bearing-adapter-authentik`](cmd/bearing-adapter-authentik) | The binaries that serve the GitHub, Kubernetes and Authentik adapters |
| [`cmd/bearing-demo-fakes`](cmd/bearing-demo-fakes) | A demo tool, not a shipped binary: serves the fakes over HTTPS and plays the Acme story on request, for [`compose.yaml`](compose.yaml) and [`docs/demo.md`](docs/demo.md) |
| [`compose.yaml`](compose.yaml), [`Dockerfile`](Dockerfile), [`demo/`](demo/) | Docker Compose demo: Bearing and the Acme fakes in one image; `demo/settings.json` is the server's settings file; `.env.example` holds the demo values |
| [`testdata/`](testdata/) | Example observations, adapter declarations, events and configuration, and the recorded Acme feeds |
| [`site/`](site/) | The project website: a small Go generator that renders an introduction, the roadmap and the spec for GitHub Pages |
| [`tools`](tools) | Pinned developer tools (golangci-lint, govulncheck, buf and its plugins) and the coverage gate, in their own Go module |

## Try it

To see the five questions answered on a fictional company, run the Docker
Compose demo: `cp .env.example .env && docker compose up -d --build`, then
follow [`docs/demo.md`](docs/demo.md).

To work on the code, you need Go 1.27.3 or later (the `go` command downloads it automatically if needed).

```sh
make check         # everything CI runs: generated code is current, lint, tests with the coverage gate, govulncheck, build
make generate      # after editing proto/: format, lint and regenerate gen/
make test          # vet and run every test
make build         # builds bin/bearing and the adapters, bin/bearing-adapter-*

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
the information came from. The core keeps each source's records, joins the
ones that describe one thing with identity links (asserted when a source or
rule vouches for them, proposed with evidence otherwise), and stores what
was said as **facts**, each with the observations behind it. See the
[roadmap](ROADMAP.md) for where this is going.

## Storage

Bearing keeps its data in DuckDB, inside the server process
([ADR 5](docs/adr/0005-duckdb-store.md), [concepts](docs/spec/concepts.md)).
`store.url` is `duckdb:///var/lib/bearing/bearing.duckdb` for a file or `mem://`
for a throwaway store. Only one process can write a DuckDB file, so while
the server runs, ask it with `--server` rather than opening the file with
`--store`.

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
