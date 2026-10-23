# Roadmap

What Bearing is building now, and ideas deliberately left for later. The
rules are in [`AGENTS.md`](AGENTS.md) and [`docs/adr/`](docs/adr/).

## Now: the MVP

Done when the five questions in `AGENTS.md` are answered end to end, with
evidence, on the Acme scenario, and Bearing runs in Docker Compose.

1. **Delete what the MVP doesn't use.** Done: Postgres, the vector index,
   the benchmark, the instance lock, the audit log and OIDC are gone; the
   API takes one static token. The stdio transport goes in step 3,
   `memstore` and the conformance suite in step 2.
2. **DuckDB store.** Done: `internal/duckstore` holds observations,
   entities, facts with provenance and the event log ([ADR 6](docs/adr/0006-small-data-model.md));
   the server, queries, API and CLI use it, and the old resolver, `memstore`
   and graph contract are gone, with their protos. [ADR 8](docs/adr/0008-ducklake-not-now.md) keeps
   the single DuckDB file as the store.
3. **Adapters over gRPC.** Port the GitHub adapter (including manifest
   parsing for languages and versions) and add the directory (IdP) and
   Kubernetes adapters against the existing fakes.
   Done: dependencies between Kubernetes workloads from env values that
   name a Service, with how (protocol, port, where) kept as qualifiers on the
   fact; the gRPC service and transport (local adapters on a private
   socket, remote ones over mutual TLS; stdio is gone), GitHub
   `runtime` facts from manifests, and the Kubernetes adapter (workloads, namespaces, `owned_by`
   from a label, `defined_in` from an annotation or image). Still needed:
   - The directory (IdP) adapter; until then the recorded
     `testdata/acme/directory.ndjson` stands in.
4. **Resolution.** Design in [ADR 7](docs/adr/0007-records-identity-links-matchers.md)
   (accepted): records, identity links and matchers, proven on the Acme set.
   Done: records and clusters in `internal/duckstore`, with authority from
   the adapter declaration deciding whether a link is asserted or proposed;
   key rules that assert links and the transparent matcher that proposes
   them, with evidence that decays (`pkg/match`, run after each sync); a
   labelled evaluation set that scores precision and recall
   (`internal/identityeval`, [results](docs/identity-evaluation.md));
   and the Acme end-to-end tests that were skipped waiting for this now pass
   (people joined by authoritative links, Tom Becker kept apart, the
   payments teams joined across systems, deleted repositories and teams
   closed when a full sync no longer lists them, ingest's `CompleteSync`).
   Still needed:
   - A person promoting or rejecting a proposal. The graph can open and
     close links (`Link`, `CloseLink`), but nothing in the server or the CLI
     calls them for a person yet.
   - The model-backed matcher (ADR 7, "Proving it out" step 3) is not built;
     it would merge only through a later ADR.
5. **Queries.** CLI, REST and MCP over one query layer, plus a
   `find_entities` lookup for names.
6. **Config.** One server file plus one block per adapter, loaded at start.
7. **Run it.** Docker Compose with the fakes; end-to-end tests for each
   question.

## Later

Each idea keeps a line on why it waits. Pre-pivot designs for several of
these are at the [`pre-pivot`](https://github.com/dhm116/bearing/tree/pre-pivot/docs/adr)
tag.

- **A conformance suite for `graph.Graph`** in `pkg/store/storetest`, moved
  from `internal/duckstore`'s behaviour tests now that ADR 7 has settled
  identity, so a second store can prove it answers the same.
- **Kubernetes and remote deployment** (kind, k3d, managed clusters;
  Bearing on a host talking to remote adapters). Same images as Compose.
- **Scale beyond one writer.** DuckDB's Quack client/server extension (beta)
  or DuckLake ([ADR 8](docs/adr/0008-ducklake-not-now.md): not yet).
- **Search.** DuckDB full-text and vector search, once a question needs
  more than name lookup.
- **Inference.** Proposed facts from text, with the quoted span as
  evidence: an extraction model (for example Cloudflare's
  [Clairic](https://blog.cloudflare.com/clairic-extraction-model), in preview
  on Workers AI, open weights announced) reads READMEs, runbooks and config
  for dependencies and how they are called, and a System One decision
  model (Kev locally, Jev hosted) checks each candidate. This is the source
  for dependencies that exist only in prose. Proposals stay separate from
  asserted facts; sending private text to a hosted model is opt-in.
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
