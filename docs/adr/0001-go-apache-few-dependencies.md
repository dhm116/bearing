# 1. Go, Apache-2.0 and few dependencies

Date: 2026-10-20 · Status: accepted

## Context

Bearing restarts from a smaller scope (see the [roadmap](../../ROADMAP.md)).
A Rust rewrite was considered and rejected: the earlier code grew complex
because of scope, not language, and Go keeps builds, tests and agent-written
code fast. The decisions made before the restart are tagged
[`pre-pivot`](https://github.com/dhm116/bearing/tree/pre-pivot/docs/adr) and
are history, not rules.

| Option | Pros | Cons |
| --- | --- | --- |
| Go | Existing code and fakes to reuse; fast builds and tests; simple for agents and junior engineers | Weaker type system than Rust |
| Rust | wasmtime and Rust LLM tooling are native; strong types | Slow cold builds; harder for cheaper models; full rewrite |

## Decision

- Write the core and first-party adapters in Go. License under Apache-2.0.
- Prefer the standard library. A new dependency needs a reason in the commit
  message; anything shipped must be Apache-2.0 compatible.
- CGO is allowed, because the store needs it ([ADR 5](0005-duckdb-store.md)).
- Tooling is Go plus POSIX shell. No other language for scripts or
  generators.

## Consequences

- Builds need a C toolchain. Cross-compiling and release images build with
  `CGO_ENABLED=1`.
- Pre-pivot code is reused only where it serves the MVP; the rest is deleted
  rather than kept beside the new code.
