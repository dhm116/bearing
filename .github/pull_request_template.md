## What changes

Before:

After:

## How

<!-- The approach, and anything a reviewer should look at first. -->

## Review

<!-- The lead posts each review round as one comment with tools/reviewcard (see docs/review-process.md, "Review comment"). -->

## Checklist

- [ ] `make check` passes
- [ ] Spec (`docs/spec/`) and schema updated if the data model, protocol or contracts changed
- [ ] ADR added for significant decisions
- [ ] New spans or metrics listed in `docs/telemetry.md`
- [ ] PostgreSQL suites run (`make test-postgres`) if `internal/pgstore` changed, or noted as not run
