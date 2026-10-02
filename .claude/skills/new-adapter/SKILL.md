---
name: new-adapter
description: Add a new Bearing source adapter (e.g. AWS, PagerDuty) that emits observations over the JSON-RPC stdio adapter protocol.
---

# Add a Bearing adapter

An adapter is a small, stateless, read-only program that reads one external
system and emits observations. `adapters/github` is the worked example; copy
its shape.

## Before writing code

1. Read `docs/spec/adapter-protocol.md` and `docs/spec/data-model.md`.
2. List the entity kinds and relations the adapter will emit, using the
   existing ones in `pkg/model/model.go`. If a new kind or relation is truly
   needed, change the data model first (spec, `pkg/model`,
   `schema/observation.v1.schema.json` together), as its own commit.
3. Decide the key format for each entity, `<system>:<type>/<id>` like
   `github:repo/acme/payments-api` (see "Keys" in the data model spec).
   Adapters never resolve identities across systems; emit the source's own
   keys and let the core link them.
4. List the minimum read-only permissions the adapter needs. They go in
   `access` in `bearing.describe` and in the package doc comment.

## Implementation checklist

- [ ] `adapters/<name>/<name>.go` with a package doc comment naming what it
      reads and the read-only access it needs.
- [ ] `const Source = "adapter/<name>"` and `const Version = "0.1.0"`.
- [ ] `Config` struct decoded from `SyncParams`/`HandleParams` config, with
      defaults applied in one place. Secrets are referenced by env var name
      (`TokenEnv`, `WebhookSecretEnv`), never passed as values.
- [ ] `Adapter` struct with injectable `HTTP *http.Client`, `Now` and
      `Getenv`, a `New()` constructor using `otelhttp.NewTransport`, and
      `var _ adapter.Adapter = (*Adapter)(nil)`.
- [ ] `Describe`: name, version, emits, config schema, access, webhooks.
- [ ] `Sync`: paged via an opaque cursor; no state kept between calls.
      Observations carry `evidence` pointing where a person can check the
      claim (the spec says SHOULD).
- [ ] `Handle` (optional): verify the webhook signature when the source
      signs, reject bad signatures, ignore unknown event types. (Under
      ADR 9 A6 the host verifies from M4; stdio adapters keep verifying
      until then, see A15.)
- [ ] `adapters/<name>/telemetry.go` for spans and metrics; log with
      `telemetry.Logger`, fail with `telemetry.Fail`, never write to stdout.
- [ ] `cmd/bearing-adapter-<name>/main.go`, copied from
      `cmd/bearing-adapter-github/main.go` (telemetry setup, then
      `adapter.ServeStdio`).
- [ ] Tests with `httptest` servers covering paging, missing config,
      upstream errors, and each webhook path. No real network.
- [ ] Add new spans and metrics to `docs/telemetry.md`.
- [ ] Add the adapter to the README's "What's here" table and "First
      adapters" list.

## Verify

```sh
make check
bin/bearing adapter describe -- bin/bearing-adapter-<name>
# with real read-only credentials, if available:
bin/bearing adapter sync --config cfg.json -- bin/bearing-adapter-<name> > obs.ndjson
bin/bearing validate obs.ndjson
```
