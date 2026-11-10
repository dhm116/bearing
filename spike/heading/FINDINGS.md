# Heading spike: findings

Spike for [ADR 12](../../docs/adr/0012-heading-adapters-from-api-specs.md),
on `spike/heading`. Never merged.

## What was run

- **Analyzer** (`analyze.go`): reads an OpenAPI 3 document and guesses
  what `heading init` would put in a mapping: which GETs list and which read
  one item, the kinds, keys, path-parameter sources, paging, references
  and sensitive fields. Output is in `testdata/out/*.report.txt` and
  `*.heading.yaml`.
- **Runner** (`run.go`): interprets a mapping (`acmeMapping` in
  `run_test.go`) against the Acme fake Kubernetes API, GET only. It is
  compared with the hand-written adapter on the same cluster, then ingested
  through the server's applier with an operator's `extends`. Output is in
  `testdata/out/kubernetes.compare.txt`.

Specs:

- **Kubernetes 1.31:** the core and apps group documents, committed in
  `testdata/` and merged, as a cluster serves them.
- **GitHub REST:** `api.github.com.json` at `github/rest-api-description`
  commit `fa8d7bf` (14 MB, not committed). To reproduce:

  ```sh
  curl -sSfLo /tmp/github-api.json https://raw.githubusercontent.com/github/rest-api-description/fa8d7bfb041cc9ef90ac51dcb8ed8cc8a0007166/descriptions/api.github.com/api.github.com.json
  HEADING_GITHUB_SPEC=/tmp/github-api.json go test ./spike/heading/
  ```

The runner interprets the mapping; it doesn't generate Go. Code generation
would template the same logic, so it changes nothing below.

## Numbers

| | Kubernetes (core + apps) | GitHub REST |
| --- | --- | --- |
| Operations (GET / other) | 145 / 187 | 664 / 608 |
| Kinds guessed (listable) | 24 (21) | 279 (164) |
| Keys guessed from the item route / from `id` / not guessed | 22 / 0 / 2 | 3 / 110 / 166 |
| List routes reachable with no config | 38 of 38 | 109 of 304 |
| Most common blocking path parameter | none | `{owner}` (123 lists) |
| Paging styles | 1 (`continue`) | 5 (page, cursor, with or without a `Link` header, none) |
| References typed by `$ref` / guessed from `*_id` | 1 / 0 | 129 / 453 |
| Fields marked sensitive (`writeOnly`, password) | 0 | 0 |

On the Acme cluster, the generated Kubernetes adapter saw every workload
the hand-written one saw. Name, kind, image, replicas and namespace
matched for all six.

## Findings

1. **Default-everything leaks secrets. This is the blocker.** Flattening
   whatever the schema allows put all 11 of the fake's canary values into
   observations: container `args`, `env` values, labels and annotations.
   The hand-written adapter leaks none. Neither spec marks a single field
   sensitive, and Kubernetes lists `Secret` and `ConfigMap` as ordinary
   kinds. Generated adapters have to **emit only allow-listed fields**,
   and deny payload kinds (`Secret`, `ConfigMap`) by default. A spec can't
   be trusted to say what is sensitive.
2. **Some fields only churn.** `resource_version`, `managed_fields`,
   `generation` and most of `status` change on every write. They would
   flood "what changed between X and Y" (MVP question 3). An allow-list
   fixes this too; otherwise the mapping needs per-API ignore defaults.
3. **Meaning can't come from the spec, as intended, but three
   relations don't come for free.** The relations the hand-written adapter
   derives are missing:
   - `owned_by`, from the `team` label: 5 of 6 workloads;
   - `defined_in`, from an annotation or the image: 4 of 6;
   - `depends_on`, from env-var URLs: 1 of 6.

   The first two are field-to-key rules, such as "label `team` names
   `Team` key `team:<value>`". They fit the operator's config, next to
   `extends`. `depends_on` parses values, so it needs code. Heading needs a
   hook for small hand-written enrichers, or that logic stays in a
   hand-written adapter beside the generated one.
4. **Kubernetes is the easy case.** It has uniform routes, keys from
   routes (namespace and name), list routes across all namespaces and one
   paging style. The `x-kubernetes-group-version-kind` and
   `x-kubernetes-action` extensions would make the guesses exact and drop
   the noise kinds the heuristics picked up: `Scale`, `WatchEvent`,
   `APIResourceList`.
5. **GitHub is the hard case.**
   - One resource has many schemas: `Repository`, `FullRepository`,
     `MinimalRepository`, `TeamRepository` and more. The mapping must fold
     these views into one kind, or Bearing gets four records per
     repository.
   - Keys are numeric `id`s, not the `owner/name` that other sources use.
   - Two thirds of the list routes need `{owner}` and similar parameters
     filled from config or another list.

   GraphQL (typed references, Relay paging, one type per object) may be
   the better source for GitHub.
6. **An embedded `$ref` is not always a reference.** Kubernetes' one hit
   was `volumeClaimTemplates`, a template with no identity. Assert a
   reference only when the embedded object carries the target's key.
7. **Guessing POST reads is unsafe.** Of 7 GitHub POSTs whose names look
   like reads ("bulk", "list"), 5 write: they delete, create or review. That
   confirms opt-in for each operation, never inference.
8. **Specs come in parts.** A cluster serves one document per
   group-version, and every CRD adds another, so Heading must merge them.
   Instance filters, such as the hand-written adapter's excluded
   namespaces, belong in config: the generated run also picked up
   `kube-system`.
9. **Extends works as designed.** With no code change, the operator's
   `extends` made 8 generated records answer as `Component` in the store.

## What this means for ADR 12

The direction holds for APIs as regular as Kubernetes. Before acceptance,
the ADR should change:

- **Default-deny attributes.** `kinds.yaml` lists every field. Only the
  ones the author ticks are emitted, and payload kinds are off by default.
- **Fold schema views** into one kind in the mapping.
- **Derive rules in operator config** (label or annotation value to a
  key in another namespace), next to `extends`.
- **A hook for code enrichers**, such as env-var dependencies, or keep
  those in small hand-written adapters.
- **Use vendor extensions when present** (`x-kubernetes-*`).
- **For GitHub, evaluate the GraphQL schema** before the REST spec.
