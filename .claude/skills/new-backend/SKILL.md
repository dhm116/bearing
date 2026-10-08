---
name: new-backend
description: Add a new backend for a Bearing contract (GraphStore, VectorIndex, etc.) and wire it into pkg/store with conformance tests.
---

# Add a backend for a Bearing contract

Each interface in `pkg/contracts` can have many backends. A backend is
accepted when it passes the interface's conformance suite. `internal/memstore`
is the simplest reference; `internal/surrealstore` shows a real database,
server and embedded modes, and one backend serving two contracts.

## Checklist

- [ ] Read the interface in `pkg/contracts/contracts.go` and its section in
      `docs/spec/contracts.md`. Behavior the spec states is the contract, not
      whatever memstore happens to do.
- [ ] Put the backend in `internal/<name>store/` with a package doc comment.
      It depends on `pkg/contracts` and `pkg/model`, nothing else in Bearing.
- [ ] Return `contracts.ErrNotFound` (wrapped is fine) for misses. Prefix
      errors with the package name.
- [ ] If the backend needs CGO or non-Apache-compatible code, put it behind
      a build tag with a stub file for the default build, as
      `internal/surrealstore/embedded.go` and `embedded_stub.go` do. The
      default binary stays CGO-free.
- [ ] Conformance test in the backend's package:
      `conformance.GraphStore(t, newStore)` and/or
      `conformance.VectorIndex(t, newIndex)`; `newStore` returns the store,
      the `conformance.Clock` it reads (a `testkit.FakeClock`) and the
      `conformance.IDs` it mints from (`testkit.NewUUIDv7s`). A backend that needs
      a live service skips unless an env var like `BEARING_TEST_<NAME>` is
      set, and gets a Makefile target like `test-surrealdb`.
- [ ] Wire a URL scheme into `pkg/store` (`open`), and add it to the package
      doc table and the README's store URL table. Passwords come from
      `BEARING_STORE_PASSWORD`, never the URL; use `redact` in errors.
- [ ] Don't add telemetry inside the backend: `pkg/store` wraps it with
      `pkg/contracts/instrument`, which gives every backend the same spans
      and metrics. Only add backend-specific metrics if they are genuinely
      new, and document them in `docs/telemetry.md`.
- [ ] Choosing a new *default* backend is an ADR (copy
      `docs/adr/template.md`).

## Changing a contract itself

Update together, in one change: `docs/spec/contracts.md`,
`pkg/contracts`, the conformance suite, the `instrument` wrapper, and every
existing backend.

## Verify

```sh
make check
make test-surrealdb SURREALDB=ws://127.0.0.1:8000 SURREALDB_USER=root SURREALDB_PASS=root   # if surrealstore changed
```
