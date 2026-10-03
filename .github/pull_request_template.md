## What changes

Before:

After:

## How

<!-- The approach, and anything a reviewer should look at first. -->

## Checklist

- [ ] `make check` passes
- [ ] Spec (`docs/spec/`) and schema updated if the data model, protocol or contracts changed
- [ ] ADR added for significant decisions
- [ ] New spans or metrics listed in `docs/telemetry.md`
- [ ] SurrealDB suites run (`make test-surrealdb` / `make test-embedded`) if `internal/surrealstore` changed, or noted as not run
