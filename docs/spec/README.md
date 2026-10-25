# Bearing specification

What callers and adapter authors can rely on. Bearing is being rebuilt from
a smaller scope (see the [roadmap](../../ROADMAP.md)), so this section is
being rewritten as each part lands.

1. [Concepts](concepts.md): observations, keys, entities, facts and their
   provenance.
2. [Server API](api.md): the routes a running server offers and their
   limits.

The adapter service is `proto/bearing/adapter/v1alpha1/adapter.proto`
([ADR 4](../adr/0004-protobuf-and-grpc-adapters.md)); writing one is in
[CONTRIBUTING](../../CONTRIBUTING.md#writing-an-adapter).

## Conventions

- The key words MUST, SHOULD and MAY are used as in RFC 2119.
- JSON field names use `snake_case`, except CloudEvents envelope fields, which
  follow the CloudEvents spec.
- Times are RFC 3339 in UTC.
