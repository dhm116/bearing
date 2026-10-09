# 5. One database for the graph and vectors to start, with SurrealDB

Date: 2026-09-29 · Status: proposed · Superseded in part by ADR 13, ADR 14

Tested: both conformance suites pass against SurrealDB 3.3.0 over
WebSocket and HTTP (`make test-surrealdb`) and embedded in memory and on
disk (`make test-embedded`).

## Context

ADR 2 keeps the graph (`GraphStore`) and the semantic index (`VectorIndex`)
behind separate contracts, with PostgreSQL and Qdrant as the defaults. That
is two databases to install, secure, back up and upgrade before anyone can
try Bearing. Most installs start small: one team, a laptop, a single VM.

Both contracts are worth keeping separate. Large installs may want a
dedicated vector engine, and the graph must stay the source of truth. What
should change is the default: one engine that serves both contracts, so the
getting-started path has one dependency, or none.

Options considered:

| Option | Graph | Vectors | Embeddable in Go | License |
| --- | --- | --- | --- | --- |
| SurrealDB | Native: edges are records with fields, traversal in SurrealQL | HNSW index | Yes, via CGO (`surrealdb.c.go`) | Core BSL 1.1, SDKs Apache-2.0 |
| PostgreSQL + pgvector | Tables and recursive CTEs (or Apache AGE) | HNSW / IVFFlat | Only by bundling a Postgres binary | PostgreSQL License |
| Separate stores (ADR 2 defaults) | PostgreSQL | Qdrant | No | Permissive |

## Decision

- **The contracts stay separate.** `GraphStore` and `VectorIndex` keep their
  own interfaces and conformance suites. A backend may implement both.
- **`pkg/store` opens backends from URLs.** `Config{Graph, Vectors}`;
  `Vectors` defaults to `Graph`, so one URL gives one database for both.
  Setting `Vectors` to another URL splits them without code changes.
- **SurrealDB is the default for both contracts** (`internal/surrealstore`):
  - Entities are records and facts are graph edges (`entity->fact->entity`)
    whose fields hold relation, confidence, assertion and sources. Fact
    history is a separate table. Vectors sit in an HNSW index and link to
    their entity, so one query can go from "closest to this text" to "owned
    by which team".
  - **Server mode** (`surrealdb+ws://`, `surrealdb+http://`) uses the
    pure-Go SDK. The default Bearing binary needs no CGO and contains no
    BSL-licensed code.
  - **Embedded mode** (`surrealdb+mem://`, `surrealkv://path`) runs SurrealDB
    in-process. It is opt-in, behind the `surrealembed` build tag, because
    it needs CGO and a Rust-built static library.
- **`mem://`** (the in-memory reference store) also serves both contracts,
  for tests and demos with no database at all.
- PostgreSQL + pgvector stays on the roadmap as the single-store option for
  organizations that standardize on Postgres. Qdrant stays the recommended
  split-out `VectorIndex` at scale.

## Consequences

- Getting started needs `mem://` (nothing to run), one `surreal start`
  process, or an embedded build.
- **License.** SurrealDB's core is BSL 1.1 with a grant that allows
  embedding and redistribution in any product; it only forbids offering
  SurrealDB itself as a commercial database service. Each release converts
  to Apache-2.0 after four years. Bearing's own code stays Apache-2.0.
  Default binaries only talk to a server through the Apache-2.0 Go SDK.
  Embedded binaries link BSL code, so their release notes must say so.
- **Embedded bindings are young.** `surrealdb.c.go` is at v0.1.0 (March 2026)
  and its module has no LICENSE file (the C library it wraps is
  Apache-2.0). This must be resolved with SurrealDB before Bearing ships
  embedded binaries. Server mode is not affected. Two quirks found in
  testing, both worked around: the embedded driver can't pass arrays of
  objects as query variables (Bearing inlines them as literals), and it
  doesn't release a `surrealkv://` file lock on Close, so reopening the same
  path needs a new process. `go mod tidy` also fails on the module's test
  dependencies.
- Building embedded binaries needs a Rust toolchain for `libsurrealdb_c.a`,
  which rules out plain `go install` and easy cross-compiling for that
  variant.
- Fewer operators know SurrealDB than PostgreSQL. Backups use
  `surreal export`; high availability uses its TiKV storage.
- The HNSW index's dimension is fixed by the first vector written, as it is
  in every vector database. Changing embedding models means re-indexing,
  which the graph can always drive (ADR 2).
- ADR 2's consequence ("two stores to run in production") becomes "one store
  by default, two when scale calls for it".
- Superseded in part by [ADR 13](0013-backends-store-rows-one-engine-applies-rules.md):
  `internal/surrealstore` stores the graph as rows, not as
  `entity->fact->entity` edges, and the memstore engine applies the rules.
- Superseded in part by [ADR 14](0014-postgres-is-the-default-store.md):
  PostgreSQL with pgvector replaces SurrealDB as the default for both
  contracts, and the SurrealDB backend, including embedded mode, is removed.
  The separate contracts, URL-based `pkg/store` and one-database default
  stand.
