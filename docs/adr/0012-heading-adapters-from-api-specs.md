# 12. Heading: adapters generated from API specs

Date: 2026-11-09 · Status: proposed

## Context

Bearing is only as useful as the systems it can read, and every adapter
today is written by hand. That does not scale:

- Most systems have no adapter, so an adopter has to write one before
  Bearing tells them anything.
- Adapters cover only part of their system. The Kubernetes adapter reads
  namespaces and three workload types; Ingresses, Jobs, CRDs and policies
  are invisible.
- Adapters decide what things *mean*. The Kubernetes adapter makes a
  namespace an `Environment`, when for most people the cluster is the
  environment. A hard-coded meaning is wrong for someone, and fixing it
  takes a code change and a release.

Most systems Bearing reads already describe their API in a machine-readable
spec. GitHub and authentik publish OpenAPI documents, and a Kubernetes
cluster serves its own at `/openapi/v3`, CRDs included. GitHub also
publishes a GraphQL schema. Generating adapters from these specs makes them:

- easy to keep current, with one adapter per spec version;
- as accurate as the vendor's own documentation;
- the same shape from one system to the next;
- easy to mock for tests and replays, from the spec's examples;
- typed, even where the API itself is loose.

A spec does not say everything an adapter needs. It doesn't say which
routes list and which read one item, where a path parameter such as
`{org}` comes from, which field identifies a thing, or how pages are
linked. It types embedded objects (`$ref`) but not ID fields such as
`owner_id`.

| Option | Pros | Cons |
| --- | --- | --- |
| Keep writing adapters by hand | Full control of the output | The adoption barrier stays; meanings stay hard-coded |
| Generate from Terraform providers, as Crossplane's upjet does | Thousands of providers | Providers manage what they're told about and rarely list; references are untyped strings |
| AI-assisted hand-writing with house conventions, as Turbot does for Steampipe | Works for any API | Still hand-shaped and still assigns meanings; needs review per table |
| **Generate from the system's own API spec (OpenAPI first, GraphQL next)** | Lists and reads are what a REST API is built for; `$ref` types references; the spec carries examples for mocks | Pagination and keys need a per-API mapping; spec quality varies |

## Decision

- **Heading**, a separate project in its own repository, generates a
  Bearing adapter from an API spec. One spec is one adapter. Heading's
  mark is the Heading concept from the Bearing logo work: a ring, a north
  tick and a needle at 040°.
- **The workflow:**

  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../diagrams/heading-workflow-dark.svg">
    <img src="../diagrams/heading-workflow.svg" alt="Swimlane of the Heading workflow. The author's OpenAPI spec is read by heading init, which writes a mapping skeleton, heading.yaml, that the author edits. heading generate turns the mapping into a generated adapter, suggested EntityKinds and test data. heading test runs the adapter against a mock API; a failure sends the author back to the mapping. A passing adapter is installed in Bearing's settings file as a Source and EntityKinds, where the operator sets extends. The Bearing server boots it and runs it over gRPC, and its observations become records such as a github.Repository, keyed github:repository/acme/payments, with an asserted owner link from a $ref and the GET route as evidence.">
  </picture>

  1. `heading init <spec>` reads the spec and writes a mapping skeleton,
     `heading.yaml`.
  2. The author edits the mapping.
  3. `heading generate` writes a Go adapter, suggested `EntityKind`
     config (`kinds.yaml`), and test data (mock responses from the spec's
     examples, plus golden observations).
  4. `heading test` runs the adapter against a mock API served from that
     test data, through the adapter protocol.
  5. An operator installs the adapter like any other: a `Source` and the
     `EntityKind`s in the settings file. The server runs it over gRPC
     (ADR 4).
- **Generated Go code**, not a runtime interpreter. Each adapter is
  generated from one pinned spec version and can be read, reviewed,
  versioned and tested like hand-written code.
- **Read-only by construction.** Generated adapters call `GET` and
  `QUERY` operations. A `POST` used for search or bulk reads is called
  only when the mapping names that operation. `PUT`, `PATCH` and `DELETE`
  are never generated. `HEAD` and `OPTIONS` carry no data, so they are
  skipped.
- **No meaning in the adapter.** Each response schema becomes a
  configured kind (ADR 10) with no parent: the spec's `repository` becomes
  `github.Repository`. The kind's namespace is one segment per API, taken
  from the mapping, else from the spec's title. Path segments that tell
  same-named schemas apart go into the kind name
  (`kubernetes.AppsV1Deployment`), because ADR 10 allows a single
  namespace segment. Whether a kind is a `Repository`, a `Component` or
  an `Environment` is the operator's `extends`. Heading never sets a
  parent.
- **The mapping** holds only what the spec doesn't say:
  - list and single-item routes, paired;
  - where each path parameter comes from: another list, or config;
  - the key field, guessed from `id` or the item route's parameter, and
    overridable;
  - pagination;
  - references beyond `$ref`. An embedded `$ref` or a declared reference
    is asserted. A guess from a naming convention (`owner_id`) is a
    proposal (ADR 2).
- **Auth and secrets.** The spec's `securitySchemes` become config fields
  that name environment variables, as for every adapter. Fields the spec
  marks `writeOnly` or as a password format are never emitted.
- The hand-written adapters stay until Heading's adapter for the same
  system answers the Acme questions as well. GitHub and Kubernetes are
  the first to compare.

## Consequences

- Covering a new system, or more of one, is a spec, a reviewed mapping and
  a generated adapter. Covering a new resource type is a spec upgrade, not
  hand-written code.
- The five MVP questions need operator mappings (`extends` to
  `Component`, `Team`, `Repository`, `Environment`) before Heading's
  records answer them. Heading should ship suggested mappings for its
  first APIs so the demo still works out of the box.
- Answers depend on spec quality. A route missing from the spec is
  invisible until the spec or the mapping adds it.
- GraphQL comes next. It types references and has standard pagination
  (Relay connections), so it needs a smaller mapping than OpenAPI.
- The cluster-as-`Environment` draft is withdrawn. That choice belongs in
  operator config.
- **Spike first,** on a `spike/heading` branch. Generate a Kubernetes
  adapter from the OpenAPI document the Acme fake serves (the fake adds
  `/openapi/v3`), and a GitHub adapter from GitHub's published spec. Check
  how much of each mapping can be guessed, and whether the records match
  what the hand-written adapters emit. This ADR is accepted, or revised,
  on what the spike finds.
