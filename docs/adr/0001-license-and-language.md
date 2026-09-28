# 1. Apache-2.0 license and Go for the core

Date: 2026-09-28 · Status: accepted

## Context

Bearing is meant to be an open-source foundation that organizations of
different sizes can adopt and extend. It needs a license companies are
comfortable running and contributing to, and a core language that is easy to
distribute and fits the cloud-native tools it integrates with.

## Decision

- License the project under the Apache License 2.0.
- Write the core and first-party adapters in Go.
- Adapters may be written in any language, because they talk to the core
  over a process protocol, not a Go API.

## Consequences

- Apache-2.0 matches Backstage, Kubernetes and most CNCF projects, and
  includes an explicit patent grant.
- Go gives single static binaries, a strong standard library for HTTP and
  JSON, and familiarity for platform engineers.
- The core keeps third-party dependencies to a minimum. OpenTelemetry is the
  first (see ADR 4).
