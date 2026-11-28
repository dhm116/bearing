# 14. One golden path for authoring adapters: a proto-defined adapter mapping in one directory

Date: 2026-11-27 · Status: accepted 2026-11-27

## Context

[ADR 12](0012-generic-openapi-adapter.md) makes adding a system a spec, a
mapping file and config, but it leaves three things open:

- **The format.** A mapping hand-written in YAML or JSON needs a schema,
  and that schema needs its own validation and docs, kept in step with the
  code by hand.
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

**Two words, two things.** This ADR's *adapter mapping* is the file that
tells the generic adapter how to read an API. ADR 13's *mappings*
(`broader`, `exact_match`, `derives`) are dated records that give terms
their meaning. An adapter mapping *suggests* ADR 13 mappings, and that is
the only link between them.

| Option | Pros | Cons |
| --- | --- | --- |
| YAML with a hand-written JSON Schema | Familiar | A second schema to keep in step with the Go types |
| A Go struct the author fills in | Typed; no schema | Every author needs Go; a mapping is no longer data |
| **A proto message, authored as YAML, checked against its descriptor** | One source for the Go types, the editor schema and the docs; same pattern as `Source` | A small YAML-to-descriptor walk to write |

## Decision

- **The adapter mapping is a proto config resource:** `AdapterMapping`
  in `proto/bearing/config/v1alpha1/adapter_mapping.proto`, with the usual
  apiVersion/kind/metadata/spec. The name doesn't say OpenAPI, because
  GraphQL and gRPC sources come later (ADR 12). Its suggestions reuse ADR
  13's `VocabularyModule` messages, and its proto comments are the
  reference docs.
- **Authors write YAML; errors name the line.** The loader decodes the
  file into a `yaml.Node` tree (`go.yaml.in/yaml/v3`) and walks it
  against the `AdapterMapping` descriptor. An unknown field or a wrong
  type is reported with its file and line, and `protojson` loads the
  result only after the walk passes. Inputs are size-limited. The
  generated JSON Schema serves editors only: `init` names it on the
  file's first line, pinned to the release tag of the `gen/jsonschema`
  file on GitHub, so editors complete and validate as you type.
- **Fields are allow-listed one leaf at a time.** A map or free-form
  field (`metadata.labels`, `annotations`, `env`, `additionalProperties`)
  is listed key by key, as in `metadata.labels.team`. Listing a whole map
  needs a `reason:`, and `check` flags it.
- **Explicit reasons, not comments.** YAML comments are for people and
  never reach the proto. Three things need a `reason:`, or `check`
  fails:
  - an emitted field whose name looks secret (`token`, `secret`,
    `password`, `credential`, `auth`, `api_key`, `private_key`, `env`,
    `args`);
  - a whole map;
  - a named `POST`, the one call that could write.
- **One directory per adapter, made by `bearing adapter init`:**

  ```
  <dir>/                 default ./adapters/<name>; --dir changes it
    adapter_mapping.yaml the AdapterMapping
    spec.lock            the spec's source and sha256
    spec.json            the spec, or a trimmed copy holding the routes used
    fixtures/            API responses that `test` replays
    testdata/            golden observations for `test`
    middleware.go        only with --middleware (ADR 12)
  ```

  The same layout serves Bearing's own adapters and an adopter's.
- **A Source names the directory explicitly.** `SourceSpec` gains
  `adapter_mapping`, a local path. It can't be a URL, and it isn't a key
  inside the free-form `settings`.
  - The server resolves the path against the settings file's directory,
    cleans it, and refuses it unless it is a directory.
  - The server applies C-OPS-3's owner-and-mode check to the directory and
    to the three files the runtime reads: `adapter_mapping.yaml`,
    `spec.lock` and `spec.json`. Write access to them decides what leaves
    a system.
  - The server then passes the absolute path to the adapter.
  - `Describe` says whether the adapter takes an adapter mapping. A Source
    that sets the field for an adapter that doesn't, or leaves it unset
    for one that does, is refused at boot, naming both.
  - The generic binary only reads these files. It never compiles or runs
    anything from the directory.
- **With middleware, the directory is a Go package.** `middleware.go`
  embeds the three runtime files by name with `go:embed`, never with a
  pattern that could pick up `fixtures/` or `testdata/`. The binary is a
  small `main` (in Bearing, under `cmd/`) that imports the package. Such
  an adapter declares in `Describe` that it takes no adapter mapping.
- **The spec is committed and pinned.** `spec.lock` holds the source and
  the sha256 of the copy that was read. Both `check` and the adapter at
  start refuse a `spec.json` that doesn't match it, naming both
  checksums. A trimmed copy is allowed, so large specs stay small, and
  `check` then works offline.
- **Credentials go only to the base URL's origin.** The base URL comes
  from the Source's settings, never from the spec or the adapter mapping.
  - Credentials are sent only to that origin (scheme, host and port).
  - Some URLs come from responses: paging (`Link`, `next`) and
    references. If one points at another origin, or downgrades `https` to
    `http`, the sync stops for that kind. The log gives the reason without
    the URL's query or userinfo.
  - Redirects are never followed across origins.
- **The adapter enforces read-only on every request it builds:** `GET`
  and `QUERY` only, plus a `POST` the adapter mapping names. The
  read-only rule needs no spec at runtime. `check` validates the same rule
  against the spec.
- **Four commands:**
  - `init <name> --spec <source>` writes the directory. Beyond ADR 12's
    guesses, it ticks identity fields (names, IDs, key fields), leaves the
    rest unticked, and flags secret-looking fields. It never writes a
    `reason:` itself.
  - `check` works offline. It walks the schema, then checks the pinned
    spec: routes, fields and keys exist, the read-only rule holds, and
    every `reason:` that's required is present and isn't empty or `TODO`.
  - `test` replays `fixtures/` with no network, prints the observations,
    and diffs them against `testdata/` (`-update` rewrites the golden).
    `--explain <kind>` traces one item: each field to its term, and each
    field the allow-list dropped. It names fields and never prints their
    values.
  - `sync` already exists, and runs against the real API.
- **Debugging is built in.** Per kind, the adapter counts items read,
  observations sent and fields dropped as metrics.
  - A dropped field is labelled by its schema path in the pinned spec,
    with map keys collapsed to `*`.
  - A field the spec doesn't describe counts as `other`, so labels never
    carry text from a response.
  - Each route call is a span (ADR 3), and all of these are listed in
    `docs/telemetry.md`.

## Consequences

- **ADR 12 is amended:**
  - this ADR chooses the format, the home and the review path, which ADR
    12 left open;
  - `init` ticks identity fields, where ADR 12 had every field unticked,
    and writes a directory, not a single file;
  - fields are allow-listed per leaf;
  - credentials are bound to the base URL's origin;
  - `test` is added beside `init`, `check` and `sync`.
- An author's loop is: `init`; edit `adapter_mapping.yaml` while the
  editor validates; `check`; `test --explain`; and `sync` last. Only
  `sync` needs the API.
- **Leak tests are built to fail.** Bearing's fixtures are recorded from
  the Acme fakes, and they carry the fakes' secret canaries: a test
  asserts that every canary is present. Observations, goldens and
  `--explain` output are checked with `testkit.AssertNoLeaks`. A leak then
  fails the test, and `test -update` can't bless it into a golden.
- Bearing's own adapter mappings are reviewed like code. Anything under
  `adapters/` already goes to the security reviewer, which suits a file
  whose field list decides what can leave a system. An adopter's directory
  gets C-OPS-3's check instead, and the threat model gains a control for
  it.
- `test` and its goldens double as the adapter's CI test, in Bearing's
  repo and in an adopter's.
- `go.yaml.in/yaml/v3` is in `go.sum` through testify and becomes a
  direct requirement of `go.mod`. ADR 13's `core.yaml` needs it anyway.
- The secret heuristic will miss some fields. Nothing is emitted unless
  ticked (ADR 12), and maps are listed per key, so a miss leaves a field
  unflagged, never sent.
- A generic adapter running remotely (ADR 4) reads a directory the server
  can't check. That is recorded in `docs/edge-cases.md`.
- **Later**, if asked:
  - **`test --record`**, to capture fixtures from a real API. It must
    keep every field the adapter mapping or a middleware reads (env vars
    for `depends_on`, paging cursors, `Link` headers, path-parameter
    sources, references). It must also strip request and response
    headers, use deterministic placeholders for everything else, and pass
    `AssertNoLeaks` against the fakes' canaries.
  - A schema hosted at `bearing.dougmccall.com`.
  - A spec cache shared across adapters.
  - A catalogue of adapter mappings.
  - `init` from a GraphQL schema or from gRPC reflection, once those
    sources exist (ADR 12).
