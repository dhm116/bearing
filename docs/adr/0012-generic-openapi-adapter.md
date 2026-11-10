# 12. One generic OpenAPI adapter, driven by a mapping

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

Most systems Bearing reads already describe their API in a spec. GitHub
and authentik publish OpenAPI documents, a Kubernetes cluster serves its
own at `/openapi/v3`, and GitHub also publishes a GraphQL schema. A spec
doesn't say everything an adapter needs, though: which routes list,
where a path parameter such as `{org}` comes from, which field
identifies a thing, or how pages are linked.

A spike on the `spike/heading` branch
([findings](https://github.com/dhm116/bearing/blob/spike/heading/spike/heading/FINDINGS.md))
read the Kubernetes and GitHub specs, REST and GraphQL. It also ran a generic
adapter, driven by a mapping file, against the Acme fake cluster. It
found:

- **The generic adapter saw every workload the hand-written one sees**,
  with the same name, kind, image, replicas and namespace.
- **Emitting whatever the schema allows leaked all 11 fake secret
  values** (container args, env values, labels). Neither spec marks a
  single field sensitive.
- **Kubernetes is regular; GitHub is not.** Kubernetes keys come from the
  routes, and every list works without config. GitHub has 279 schemas,
  several per resource, numeric IDs, and most lists need parameters
  filled in.
- **Some relations need code.** `depends_on`, parsed from env-var URLs, is
  one. Others are simple field rules: team ownership from a label, the
  repository from an annotation.
- **Guessing which POSTs read is unsafe:** 5 of the 7 that looked like
  reads write.

| Option | Pros | Cons |
| --- | --- | --- |
| Keep writing adapters by hand | Full control | The adoption barrier stays; meanings stay hard-coded |
| Generate Go code per API from the spec | A reviewable artifact per API | A build and a release per API; safety rules repeated in every generated adapter |
| **One generic adapter that runs a mapping, with Go middleware for what a mapping can't say** | Adding a system is config; read-only and the field allow-list are enforced once; custom logic is still plain Go | Every API rides on one adapter's quality; a mapping can't express everything |

## Decision

- **One generic adapter**, `adapters/openapi`, served as
  `bearing-adapter-openapi`. It is configured with a pinned spec and a
  mapping, and speaks the adapter protocol like any other adapter
  (ADR 4).

  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../diagrams/adapter-workflow-dark.svg">
    <img src="../diagrams/adapter-workflow.svg" alt="Swimlane of the adapter workflow. bearing adapter init reads the author's OpenAPI spec and writes a mapping skeleton, mapping.yaml, which the author edits. The generic OpenAPI adapter runs the mapping, GET only, emitting listed fields only, optionally with the author's Go middleware registered through Use. bearing adapter check and sync test it against a mock API; a failure sends the author back to the mapping. A passing adapter is installed in Bearing's settings as a Source with its mapping and EntityKinds, where the operator sets extends. The server runs it over gRPC, and its observations become records such as a github.Repository keyed github:repository/acme/payments, with an asserted owner link from a $ref and the GET route as evidence.">
  </picture>
- **`bearing adapter init <spec>`** writes a mapping skeleton and
  suggested `EntityKind` config from the spec. It guesses list and
  single-item routes, keys from routes or `id`, path-parameter sources,
  paging and references. Anything it can't guess is marked TODO.
  **`bearing adapter check`** validates a mapping against its spec.
  **`bearing adapter sync`**, which exists already, runs it against a
  mock server or a real API.
- **What the mapping holds:**
  - which kinds to read;
  - the list route for each, with where its parameters come from;
  - the key;
  - paging;
  - references;
  - which schemas are views of one kind (GitHub's `repository`,
    `full-repository` and `minimal-repository` become one
    `github.Repository`);
  - the **fields to emit. Nothing is emitted unless the mapping lists
    it.** `init` lists every field unticked, and payload kinds such as
    Kubernetes `Secret` and `ConfigMap` are left out.
- **Read-only by construction.** The adapter calls `GET` and `QUERY`
  operations only, plus a `POST` only when the mapping names that
  operation. It never calls `PUT`, `PATCH` or `DELETE`.
- **Webhooks from the spec.** OpenAPI 3.1 describes webhooks. GitHub's
  3.1 spec lists 275, with their event and signature headers, and their
  payloads use the same schemas as the REST routes. The generic adapter
  implements `Handle` (ADR 4) from the mapping:
  - which events to take, by event header and `action`;
  - which payload fields hold which kinds: `repository` is a
    `github.Repository`, built with that kind's key, field allow-list,
    views and middleware, exactly as in a sync;
  - which actions end a record (`deleted`).

  The mapping names the signature scheme (`X-Hub-Signature-256`, HMAC
  SHA-256) for the adapter's declaration, and the server's webhook route
  verifies it as it does now. The webhook spec can differ from the fetch
  source: GitHub could sync over GraphQL and take webhooks described by
  its REST spec. APIs with no described webhooks (OpenAPI 3.0, Kubernetes)
  are sync-only. Kubernetes watch streams are out of scope.
- **No meaning in the adapter.** Each kind is a configured kind (ADR 10)
  named for its API, such as `kubernetes.Deployment`, with no parent.
  Whether it is a `Component` or an `Environment` is the operator's
  `extends`. An embedded `$ref` is asserted as a reference only when the
  embedded object carries the target's key. A reference guessed from a
  field name (`owner_id`) is a proposal (ADR 2).
- **Middleware for the rest.** The adapter is also a Go package. An
  author who needs more writes a small `main` that registers functions,
  in the style of Go HTTP middleware:

  ```go
  a := openapi.New(spec, mapping)
  a.Use("Deployment", envDependencies) // func(next openapi.Handler) openapi.Handler
  adapter.Main(a)
  ```

  A handler gets the raw item and the observation built from it. It may
  add relations, add attributes or drop the item. After every handler
  runs, the adapter checks the observation again: only mapped or declared
  fields leave the process. A middleware may read a secret in order to
  parse it, but can't send it on by accident.
- **Derive rules in config.** Simple rules, such as "label `team` names
  `Team` key `team:<value>`", go in the operator's config next to
  `extends`, not in Go.
- **The schema source is a seam.** Only two things depend on the spec
  format:
  - reading the spec, so `init` and `check` can guess and validate;
  - fetching a kind's items: an HTTP route with its paging, a GraphQL
    query with Relay connections, or a gRPC `List` method with
    `page_token`.

  Everything else is shared, whatever the source:
  - the mapping's kinds, keys, field allow-list, references and views;
  - building observations;
  - middleware;
  - the read-only rule;
  - the final field check.

  Each source states its own read-only rule: `GET`/`QUERY` for OpenAPI,
  queries but never mutations for GraphQL, and for gRPC only methods
  marked `idempotency_level = NO_SIDE_EFFECTS` or named in the mapping.
  OpenAPI is built first. GraphQL comes next, for APIs like GitHub where
  REST needs a large mapping; gRPC when a system needs it.
- The hand-written adapters stay until the generic adapter, with
  middleware, answers the Acme questions as well. Kubernetes goes first:
  a mapping plus an env-dependency middleware should replace it.

## Consequences

- **Adding a system, or more of one, is a spec, a reviewed mapping, and
  config.** Go is needed only for middleware.
- **Safety is fixed in one place.** Read-only, the field allow-list and
  secret handling are reviewed once for every API.
- **Mappings and their pinned specs are versioned artifacts.** They need
  a home and a review path; this ADR does not choose one.
- **The five MVP questions need operator config before a generic
  adapter's records answer them:** `extends` to `Component`, `Team`,
  `Repository` and `Environment`, plus derive rules. Bearing should ship
  suggested config for its first APIs, so the demo still works out of the
  box.
- **The cluster-as-`Environment` draft and the separate Heading project
  are dropped.**
- **Follow-up:**
  - `adapters/openapi` with middleware, and `Handle` for described
    webhooks;
  - `bearing adapter init` and `check`;
  - derive rules in config;
  - the Kubernetes mapping;
  - an end-to-end test comparing it with the hand-written adapter on the
    Acme scenario.
