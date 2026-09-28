# Bearing

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
| [`docs/spec/`](docs/spec/) | The specification: data model, adapter protocol, component contracts |
| [`docs/adr/`](docs/adr/) | Architecture decision records |
| [`schema/observation.v1.schema.json`](schema/observation.v1.schema.json) | JSON Schema for the observation envelope |
| [`pkg/model`](pkg/model) | Go types for entity kinds, relations and observations |
| [`pkg/adapter`](pkg/adapter) | The adapter protocol: server helper for adapter authors, client for the core |
| [`pkg/contracts`](pkg/contracts) | Interfaces between components (graph store, vector index, judge, policy, executor, …) |
| [`pkg/contracts/conformance`](pkg/contracts/conformance) | Test suites every backend must pass |
| [`internal/memstore`](internal/memstore) | In-memory graph store, the reference implementation |
| [`adapters/github`](adapters/github) | The GitHub adapter |
| [`cmd/bearing`](cmd/bearing) | Developer CLI for running and checking adapters |

## Try it

Requires Go 1.24 or later.

```sh
make test          # vet and run every test
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

## First adapters

- **GitHub**: repositories, CODEOWNERS, teams, members (this repo)
- **AWS**: accounts, tagged resources, CloudTrail changes (planned)
- **PagerDuty**: services, schedules, escalation policies, incidents (planned)

## Module path

The Go module path `bearing.example` is a placeholder until the project has
a home. Changing it later is a single find-and-replace.

## License

Apache License 2.0. See [LICENSE](LICENSE).
