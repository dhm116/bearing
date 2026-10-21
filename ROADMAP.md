# Roadmap

What Bearing is building now, and ideas deliberately left for later. The
rules are in [`AGENTS.md`](AGENTS.md) and [`docs/adr/`](docs/adr/).

## Now: the MVP

Done when the five questions in `AGENTS.md` are answered end to end, with
evidence, on the Acme scenario, and Bearing runs in Docker Compose.

1. **Delete what the MVP doesn't use.** `pkg/audit` and the `audit`
   commands; audit checkpoints and retention in `pkg/server`;
   `internal/pgstore`; `internal/memstore` once the DuckDB store replaces
   it; `pkg/auth` (OIDC); the instance lock; the stdio adapter transport;
   `cmd/bearing-bench`; `pkg/contracts/conformance`; the docs that describe
   them (`docs/spec/`, `docs/security/threat-model.md`,
   `docs/operations.md`, `docs/benchmarks/`). Trim `proto/` to
   observations, the adapter service and events.
2. **DuckDB store.** Observation log, entities, facts with provenance;
   batched writes; recursive CTEs for dependency walks.
3. **Adapters over gRPC.** Port the GitHub adapter (including manifest
   parsing for languages and versions) and add the directory (IdP) and
   Kubernetes adapters against the existing fakes.
4. **Resolution.** Deterministic rules first: CODEOWNERS, team membership,
   Kubernetes labels and owner references, package manifests. Inferred
   links are stored as proposals.
5. **Queries.** CLI, REST and MCP over one query layer, plus a
   `find_entities` lookup for names.
6. **Config.** One server file plus one block per adapter, loaded at start.
7. **Run it.** Docker Compose with the fakes; end-to-end tests for each
   question.

## Later

Each idea keeps a line on why it waits. Pre-pivot designs for several of
these are at the [`pre-pivot`](https://github.com/dhm116/bearing/tree/pre-pivot/docs/adr)
tag.

- **Kubernetes and remote deployment** (kind, k3d, managed clusters;
  Bearing on a host talking to remote adapters). Same images as Compose.
- **Scale beyond one writer.** DuckDB's Quack client/server extension (beta)
  or DuckLake.
- **Search.** DuckDB full-text and vector search, once a question needs
  more than name lookup.
- **Inference.** Models that classify events and propose links with
  evidence (for example Rig, or small extraction models). Proposals stay
  separate from asserted facts.
- **Attestation.** Signed or tamper-evident provenance, built on the
  provenance facts already carry. Replaces the dropped audit log if needed.
- **Automation.** Actions triggered by data type, value or confidence.
- **WASM adapter sandboxing**, with host-held credentials and capabilities.
- **Authentication** (OIDC) for the API, beyond a static token.
- **Live config reload**, more formats, and pushing config to adapters.
- **Secrets management** for adapters and automation.
- **GraphQL API.**
- **Caching.**
- **More adapters:** AWS, PagerDuty.
