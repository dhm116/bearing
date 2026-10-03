# Bearing specification

Version 0.2 (draft). 0.2 rewrites the [data model](data-model.md) around
stable subjects and facts over time; see its "Changes in this revision".
[Contracts](contracts.md) and the [adapter protocol](adapter-protocol.md)
still describe 0.1 and are updated when the data model is implemented.

The specification is the product. Code in this repository implements it, but
anyone should be able to write an adapter or a backend from these documents
alone.

1. [Data model](data-model.md): subjects, aliases, facts over time, observations and resolver rules
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
  anything requires `v2`, except that before spec 1.0 a draft may make
  listed incompatible changes within `v1` (as 0.2 does, below).
- The specification as a whole has a draft version (0.1, 0.2, …) that
  changes whenever its rules change. Rules that change what the core builds
  from the same observations (resolution, ordering, confidence) change the
  spec version even when the observation format does not. 0.2 keeps the
  observation type `dev.bearing.observation.v1` despite a few listed
  incompatibilities (see the data model's "Compatibility"), which pre-1.0
  allows; the Protobuf model (issue #11) follows this document.
- The adapter protocol has its own version (`0.1`), reported by adapters in
  `bearing.describe`. Until `1.0`, minor versions may break compatibility.
