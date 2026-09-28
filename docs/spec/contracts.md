# Component contracts

Bearing's core is a set of components that talk only through the interfaces
below. Each has one default backend; any other backend that passes the
interface's conformance suite can replace it. The Go definitions are in
[`pkg/contracts`](../../pkg/contracts/contracts.go).

| Interface | Responsibility | Default | Alternatives | Conformance suite |
| --- | --- | --- | --- | --- |
| `GraphStore` | Entities, aliases and facts with history. The source of truth. | PostgreSQL | Neo4j, Apache AGE, Memgraph | Yes (`conformance.GraphStore`) |
| `VectorIndex` | Semantic search over entities and documents, keyed by graph ID | Qdrant | pgvector, OpenSearch, Weaviate | Planned |
| `EventBus` | At-least-once delivery of CloudEvents between components | NATS JetStream | Kafka, SQS/SNS, Postgres queue | Planned |
| `Extractor` | Proposes candidate entities and relations from unstructured text | Any chat-completions style LLM API | Hosted or local models | Planned |
| `Judge` | Calibrated typed judgments: choice, yes/no, score | Kev 4B, self-hosted | Jev hosted API | Planned |
| `PolicyDecider` | Allow or deny an action, with the reason and how to fix it | Open Policy Agent | Cedar | Planned |
| `Executor` | Plan, apply, verify and roll back actions durably | Temporal | Postgres-backed job runner | Planned |

Beyond Go interfaces, components that run as separate services expose the
same operations over a network protocol (gRPC or HTTP+JSON), so a backend can
be written in any language. Those wire definitions will live in `proto/`.

## Rules that apply to every component

- **The graph is the source of truth.** The vector index, caches and search
  are derived from it and can be rebuilt from it. Policy and ownership
  answers read only asserted graph facts.
- **Candidates are not facts.** Anything an `Extractor` proposes goes through
  a `Judge` before it reaches the graph.
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

[`internal/memstore`](../../internal/memstore) is the reference
`GraphStore` and shows the pattern.
