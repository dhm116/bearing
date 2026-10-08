# Component contracts

Bearing's core is a set of components that talk only through the interfaces
below. Each has one default backend; any other backend that passes the
interface's conformance suite can replace it. The Go definitions are in
[`pkg/contracts`](../../pkg/contracts/contracts.go).

| Interface | Responsibility | Default | Alternatives | Conformance suite |
| --- | --- | --- | --- | --- |
| `GraphStore` | Entities, aliases and facts with history. The source of truth. | SurrealDB | PostgreSQL, Neo4j, Apache AGE, Memgraph | Yes (`conformance.GraphStore`) |
| `VectorIndex` | Semantic search over entities and documents, keyed by graph ID | SurrealDB | Qdrant, pgvector, OpenSearch, Weaviate | Yes (`conformance.VectorIndex`) |
| `EventBus` | At-least-once delivery of CloudEvents between components | NATS JetStream | Kafka, SQS/SNS, Postgres queue | Planned |
| `Extractor` | Proposes candidate entities and relations from unstructured text | A self-hosted model behind a chat-completions style API (never a hosted LLM API by default) | Hosted models, only with a per-provider `insecure_hosted_model_<provider>` setting | Planned |
| `Judge` | Calibrated typed judgments: choice, yes/no, score | Kev 4B, self-hosted | Jev hosted API | Planned |
| `PolicyDecider` | Allow or deny an action, with the reason and how to fix it | Open Policy Agent | Cedar | Planned |
| `Authorizer` | Allow or deny a caller's request to Bearing by role, with the reason ([ADR 12](../adr/0012-authentication-through-oidc.md)) | OIDC group and client-ID to role mapping | OpenFGA, SpiceDB | Planned |
| `Executor` | Plan, apply, verify and roll back actions durably | Temporal | Postgres-backed job runner | Planned |

## One database to start

A backend can serve more than one interface. By default SurrealDB backs
both `GraphStore` and `VectorIndex`, so a small install runs one database, or
none with an embedded build ([ADR 5](../adr/0005-one-store-to-start.md)).
[`pkg/store`](../../pkg/store) opens backends from URLs:

| URL | Backend | Needs |
| --- | --- | --- |
| `mem://` | In-memory reference store | Nothing; data is lost on exit |
| `surrealdb+ws://user@host:8000?ns=bearing&db=main` | SurrealDB server (also `wss`, `http`, `https`) | A running `surreal start` |
| `surrealdb+mem://` | Embedded SurrealDB in memory | A `-tags surrealembed` build (CGO) |
| `surrealkv:///var/lib/bearing` | Embedded SurrealDB on disk | A `-tags surrealembed` build (CGO) |

`store.Config{Graph: url}` uses one backend for both. Setting
`Config.Vectors` to a second URL splits them, for example a SurrealDB graph
with Qdrant vectors, and nothing else changes. Passwords come from
`BEARING_STORE_PASSWORD`, not the URL; opening a URL that carries a password
MUST fail.

In a vector index, a point's kind is its `kind` payload field, which
`VectorQuery.Kinds` filters on.

Beyond Go interfaces, components that run as separate services expose the
same operations over a network protocol (gRPC or HTTP+JSON), so a backend can
be written in any language. Those wire definitions will live in `proto/`.

## Rules that apply to every component

- **The graph is the source of truth.** The vector index, caches and search
  are derived from it and can be rebuilt from it. Policy and ownership
  answers read only asserted graph facts.
- **Candidates are not facts.** Anything an `Extractor` proposes goes through
  a `Judge` before it reaches the graph.
- **No hosted LLM by default.** Source text is untrusted and often private,
  so no default component, including the default `Extractor`, MUST send it
  to a hosted LLM or embedding API. An operator enables a hosted provider
  explicitly, per provider, with its config listing the fields sent (threat
  model C-MODEL-1, C-MODEL-2).
- **Idempotent consumers.** Event delivery is at least once. Handlers must
  tolerate duplicates.
- **Least privilege.** Components that read external systems (adapters) hold
  read-only credentials. Only the `Executor` holds write credentials, and
  only after a `PolicyDecider` allows the action.

## Adding a backend

1. Implement the interface.
2. Call the conformance suite from your backend's tests:

   ```go
   func TestConformance(t *testing.T) {
       conformance.GraphStore(t, func(t *testing.T) contracts.GraphStore {
           return newEmptyStoreForTest(t)
       })
   }
   ```

3. Document any behavior the suite doesn't cover (consistency, limits).

[`internal/memstore`](../../internal/memstore) is the reference `GraphStore`
and `VectorIndex` and shows the pattern.
[`internal/surrealstore`](../../internal/surrealstore) shows one backend
passing both suites.
