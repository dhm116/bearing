# Proposed architecture (ADRs 6–10)

How the proposals in [ADR 6](adr/0006-protobuf-contracts.md) through
[ADR 10](adr/0010-configuration-as-resources.md) fit together. This is a
proposal; the current code still follows ADRs 1–5.

```mermaid
flowchart TB
  SRC["Source systems<br/>GitHub · AWS · PagerDuty"]
  PEOPLE["People · agents · CLI"]

  subgraph bearing["bearing server: one binary, or scaled out"]
    direction TB
    ING["Ingest<br/>webhooks · API"]
    SCH["Scheduler"]
    LOG[("Event log<br/>WAL · ADR 7")]
    subgraph workers["Workers"]
      direction LR
      RT["WASM adapters<br/>ADR 9"] --> CAP["Capabilities<br/>http · log · trace · kv · config"]
      RES["Resolve identities<br/>score facts"]
    end
    subgraph store["Store: SurrealDB by default · ADR 5"]
      direction LR
      G[("Graph")]
      V[("Vectors")]
      A[("Audit · ADR 8")]
      C[("Config · ADR 10")]
    end
    APIQ["Query and config API<br/>Connect · ADR 6"]
  end

  SRC -- "webhooks" --> ING
  ING -- "append, then ack" --> LOG
  SCH -- "SyncRequested" --> LOG
  LOG --> workers
  CAP -- "allowlisted HTTP" --> SRC
  workers -- "Apply: one transaction" --> store
  PEOPLE --> APIQ
  APIQ --> store
```

Protobuf ([ADR 6](adr/0006-protobuf-contracts.md)) defines every arrow that
crosses a component boundary: events on the log, the adapter interface,
capabilities, config resources, audit records and the query API.

| Concern | Standalone | Distributed |
| --- | --- | --- |
| Event log | Table in the store | NATS JetStream (or Kafka) |
| Store | Embedded or single SurrealDB | SurrealDB cluster, or split graph and vectors |
| Adapters | In-process WASM | WASM on any worker; remote adapters for what WASM can't host |
| Capabilities | In-process | In-process, or remote providers (shared egress, cache) |
| Config | `bearing server --config ./config` | `bearing apply` / API into the store |
