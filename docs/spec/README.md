# Bearing specification

What callers and adapter authors can rely on. Bearing is being rebuilt from
a smaller scope (see the [roadmap](../../ROADMAP.md)), so this section is
being rewritten as each part lands.

1. [Data model](data-model.md): subjects, facts over time and observations.
   It still describes the code from before the restart; a short concepts
   page replaces it with the DuckDB store.
2. [Server API](api.md): the routes a running server offers and their
   limits.

The adapter service is defined in `proto/` and gets a guide when the gRPC
adapters land ([ADR 4](../adr/0004-protobuf-and-grpc-adapters.md)).

## Conventions

- The key words MUST, SHOULD and MAY are used as in RFC 2119.
- JSON field names use `snake_case`, except CloudEvents envelope fields, which
  follow the CloudEvents spec.
- Times are RFC 3339 in UTC.
