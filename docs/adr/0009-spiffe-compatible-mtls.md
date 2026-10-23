# 9. SPIFFE-compatible mutual TLS; Bearing issues no identities

Date: 2026-10-23 · Status: accepted

## Context

Remote adapters and the core already authenticate each other with mutual
TLS from PEM files ([ADR 4](0004-protobuf-and-grpc-adapters.md)). Two gaps
remained: the files were read once, so short-lived certificates (SPIRE
rotates SVIDs about hourly) needed restarts; and any certificate from the
configured CA was accepted, so that CA had to be dedicated to Bearing.

| Option | Pros | Cons |
| --- | --- | --- |
| Bearing issues identities (its own CA) | One tool to run | A CA to build, secure and rotate; competes with what platforms already run |
| go-spiffe and the Workload API | Native SPIRE, no files | A dependency and a SPIRE-only path; files still needed for other CAs |
| Files any CA writes, plus an optional SPIFFE ID check | Works with SPIRE (via `spiffe-helper`), cert-manager or a hand-run CA; no dependency | Rotation is by polling file metadata |

## Decision

- Mutual TLS stays standard and file based. SPIRE, or any CA, issues
  identities; Bearing doesn't.
- Both sides re-read their certificate, key and CA when a file changes (at
  most once a second); a failed reload keeps the last good files.
- Peer identity by SPIFFE ID is optional (`tls.peer_id`,
  `--tls-peer-id`, `BEARING_ADAPTER_TLS_PEER_ID`). When set, the peer's
  certificate must hold exactly that one URI SAN, which is stronger than
  "any certificate from the CA". The core checks it in place of a host
  name and verifies the chain itself, since SVIDs carry no DNS names.

## Consequences

- A SPIRE trust domain can be shared with other workloads once peer IDs
  are set; without them, the client CA must still issue only Bearing core
  certificates.
- go-spiffe's Workload API (no files on disk) and OpenZiti are left for
  later. Ziti works today as a tunnel in front of an adapter, with no code
  change.
- The API listener's certificate is still read once
  ([edge cases](../edge-cases.md)).
