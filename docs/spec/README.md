# Bearing specification

Version 0.1 (draft).

The specification is the product. Code in this repository implements it, but
anyone should be able to write an adapter or a backend from these documents
alone.

1. [Data model](data-model.md): entity kinds, relations, keys and observations
2. [Adapter protocol](adapter-protocol.md): how adapters and the core talk
3. [Component contracts](contracts.md): the interfaces between core components

## Conventions

- The key words MUST, SHOULD and MAY are used as in RFC 2119.
- JSON field names use `snake_case`, except CloudEvents envelope fields, which
  follow the CloudEvents spec.
- Times are RFC 3339 in UTC.

## Versioning

- The data model is versioned by observation type
  (`dev.bearing.observation.v1`). Adding kinds, relation types or optional
  fields is backwards compatible. Removing or changing the meaning of
  anything requires `v2`.
- The adapter protocol has its own version (`0.1`), reported by adapters in
  `bearing.describe`. Until `1.0`, minor versions may break compatibility.
