# 14. One golden path for authoring adapters: a proto-defined mapping in one directory

Date: 2026-11-27 · Status: accepted 2026-11-27

## Context

[ADR 12](0012-generic-openapi-adapter.md) makes adding a system a spec, a
mapping and config, but it leaves three things open:

- **The mapping's format.** A mapping hand-written in YAML or JSON needs a
  schema, and that schema needs its own validation and docs, kept in step
  with the code by hand.
- **Where it lives.** ADR 12 says mappings and their pinned specs "need a
  home and a review path; this ADR does not choose one." With no stated
  home, every author has to ask where the files go.
- **How an author knows it works.** The only check so far is running
  `bearing adapter sync` against a real API. ADR 12's spike leaked all 11
  fake secrets when it emitted everything the schema allowed. An author
  needs to see what is sent, and what is dropped, before any sync runs.

The goal is an adapter that's easy to write: explicit rather than
implicit, one opinionated path that still allows changes, automation
wherever it gives the same result every time, and debugging built in.

Bearing already defines its config in protobuf: `Source` in
`proto/bearing/config/v1alpha1/resources.proto`, in the
apiVersion/kind/metadata/spec shape. `make generate` already writes a
JSON Schema for every message into `gen/jsonschema/`.

| Option | Pros | Cons |
| --- | --- | --- |
| YAML with a hand-written JSON Schema | Familiar | A second schema to keep in step with the Go types |
| A Go struct the author fills in | Typed; no schema | Every author needs Go; a mapping is no longer data |
| **A proto message, authored as YAML, checked against its descriptor** | One source for the Go types, the editor schema and the docs; same pattern as `Source` | A small YAML-to-descriptor walk to write |

## Decision

- **The mapping is a proto config resource:** `Mapping` in
  `proto/bearing/config/v1alpha1/mapping.proto`, kind `Mapping`, with the
  usual apiVersion/kind/metadata/spec. Its suggested mappings reuse ADR
  13's `VocabularyModule` messages, and its proto comments are the
  reference docs.
- **Authors write YAML; errors name the line.** The loader decodes the
  file into a `yaml.Node` tree (`go.yaml.in/yaml/v3`) and walks it
  against the `Mapping` descriptor. An unknown field or a wrong type is
  reported with its file and line. `protojson` loads the result only after
  the walk passes. The generated JSON Schema serves editors only: `init`
  names it on the file's first line, pinned to the release tag of the
  `gen/jsonschema` file on GitHub, so editors complete and validate as you
  type.
- **Explicit reasons, not comments.** YAML comments are for people and
  never reach the proto. A field the mapping emits whose name looks secret
  (`token`, `secret`, `password`, `api_key`, `private_key`, `env`,
  `args`) needs a `reason:`, or `check` fails.
- **One directory per adapter, made by `bearing adapter init`:**

  ```
  <dir>/                 default ./adapters/<name>; --dir changes it
    mapping.yaml         the Mapping
    spec.lock            the spec's source and sha256
    spec.json            the spec, or a trimmed copy holding the routes used
    fixtures/            API responses that `test` replays
    testdata/            golden observations for `test`
    middleware.go        only with --middleware (ADR 12)
  ```

  The same layout serves Bearing's own adapters and an adopter's.
  - **Without middleware**, a `Source` names the directory
    (`mapping: adapters/pagerduty`), and `bearing-adapter-openapi` runs
    it.
  - **With middleware**, the directory is a Go package. `middleware.go`
    embeds `mapping.yaml` and `spec.lock` with `go:embed` and registers
    its handlers. The binary is a small `main` (in Bearing, under `cmd/`)
    that imports the package, so the image doesn't have to ship the
    directory.
- **The spec is committed and pinned.** `spec.lock` holds the source and
  the sha256 of the copy that was read; `check` refuses a `spec.json` that
  doesn't match it, naming both checksums. A trimmed copy is allowed, so
  large specs stay small, and `check` then works offline.
- **Four commands:**
  - `init <name> --spec <source>` writes the directory. Beyond ADR 12's
    guesses, it ticks identity fields (names, IDs, key fields), leaves the
    rest unticked, and flags secret-looking fields.
  - `check` works offline. It walks the schema, then checks the pinned
    spec: routes, fields and keys exist, the read-only rule holds, and
    every secret-looking field that is emitted has a `reason`.
  - `test` replays `fixtures/` with no network, prints the observations,
    and diffs them against `testdata/` (`-update` rewrites the golden).
    `--explain <kind>` traces one item: each field to its term, and each
    field the allow-list dropped. It names fields and never prints their
    values.
  - `sync` already exists, and runs against the real API.
- **Debugging is built in.** Per kind, the adapter reports the items read,
  the observations sent and the fields the allow-list dropped, labelled
  by mapped field path, never by map keys such as label names. Each route
  call is a span (ADR 3). All of these are listed in `docs/telemetry.md`.

## Consequences

- **ADR 12 is amended:** this ADR chooses the mapping's format, its home
  and its review path, which ADR 12 left open, and adds `test` beside its
  `init`, `check` and `sync`.
- An author's loop is: `init`; edit `mapping.yaml` while the editor
  validates; `check`; `test --explain`; and `sync` last. Only `sync` needs
  the API.
- Bearing's own mappings are reviewed like code. Anything under
  `adapters/` already goes to the security reviewer, which suits a file
  whose field list decides what can leave a system.
- `test` and its goldens double as the adapter's CI test, in Bearing's
  repo and in an adopter's.
- `go.yaml.in/yaml/v3` becomes a direct requirement. It is already in
  `go.sum` through testify, and ADR 13's `core.yaml` needs it anyway.
- The secret heuristic will miss some fields. Nothing is emitted unless
  ticked (ADR 12), so a miss leaves a field unflagged, never sent.
- Fixtures are written by hand or from a fake server for now. Bearing's
  are recorded from the Acme fakes, whose values are fake and checked
  with `testkit.AssertNoLeaks`.
- **Later**, if asked:
  - **`test --record`**, to capture fixtures from a real API. It must
    keep every field the mapping or a middleware reads (env vars for
    `depends_on`, paging cursors, `Link` headers, path-parameter sources,
    references), strip request and response headers, and use
    deterministic placeholders for everything else.
  - A schema hosted at `bearing.dougmccall.com`.
  - A spec cache shared across adapters.
  - A catalogue of mappings.
  - `init` from a GraphQL schema or from gRPC reflection, once those
    sources exist (ADR 12).
