# 12. Authentication goes through OIDC and a local socket; Bearing issues no credentials

Date: 2026-10-08 · Status: proposed

## Context

Bearing answers questions about who owns what, so it needs to know who is
asking. It will be called by people (CLI, a future UI), by automation and by
AI agents, and a deployment may run on one laptop or behind a shared
gateway.

Building login into Bearing would make it a credential store: passwords,
API keys or session tokens that an attacker can steal from Bearing's own
database. Organizations already run an identity provider, and the
[threat model](../security/threat-model.md) (B4, B5) was written against
that assumption. This ADR records the decision that the threat model and
the MVP plan already rely on.

| Option | Pros | Cons |
| --- | --- | --- |
| Bearing issues API keys or tokens | Works with no identity provider | A credential store to protect, rotate and audit; a second user directory |
| Delegate to the organization's OIDC provider, plus a local Unix socket | No credentials held; reuses SSO, groups and client credentials; works offline on one machine | Needs an OIDC provider for remote access; the socket is local only |
| Put a proxy in front and trust its headers | Simple in Bearing | Anyone who can reach Bearing around the proxy is trusted |

## Decision

- **Bearing never mints, issues, refreshes or stores credentials.** There
  are no local users, passwords or API keys (C-IDP-4). It validates what an
  identity provider issued and nothing more.
- **Remote access is OIDC.** The TCP API listener is off by default and
  refuses to start unless an OIDC issuer is configured; there is no
  setting for unauthenticated access, apart from the health endpoints,
  which return no data (C-API-2). MCP over HTTP likewise requires OIDC
  (C-MCP-3). Bearing verifies bearer access tokens against the issuer's
  JWKS (C-IDP-1, C-IDP-2). People sign in at the identity provider, and
  agents and automation use its client credentials flow.
- **Local access is a Unix socket** in a private directory, readable only by
  the service user. The server accepts only the service user's UID and root,
  and an accepted local caller is an administrator recorded as
  `local:<uid>` (C-API-1). The socket keeps working when the identity
  provider is down.
- **Roles come from the identity provider.** Only the configured groups
  claim, or the client ID for a client-credentials token, maps to a role
  (C-IDP-3). The roles are `read`, `ingest` (for named Sources) and `admin`;
  an unmapped caller is denied, an agent gets `read` only, and a client ID
  may map to `read` or `ingest` but never `admin`; config apply rejects
  that mapping (C-API-4).
- **An `Authorizer` contract decides every request.** It takes the verified
  caller (issuer, subject or client ID, groups), the method and, where it
  applies, the Source, and returns allow or deny with a reason
  ([contracts](../spec/contracts.md#authorizer)). Each API
  method declares its required role in one table, and a method with no
  entry is denied (C-API-5). The default backend is the group-to-role
  mapping above. Relationship-based backends such as OpenFGA or SpiceDB can
  replace it after the MVP if they pass the contract's conformance suite.
  `Authorizer` answers who may call Bearing; `PolicyDecider` answers
  whether an action on the outside world may run, and the two stay
  separate.
- **Ingest authentication is separate.** A pushed event is authenticated by
  the host for its Source (ADR 7, C-INGEST-2); an OIDC access token mapped
  to `ingest` for that Source is one of the methods it can use.
- **Bearing never forwards a caller's token.** It reads source systems only
  with its own configured credentials (C-MCP-4).

## Consequences

- A remote deployment needs an OIDC provider. A single-machine deployment
  needs none, because the socket is enough.
- A local socket caller is an administrator, so a local-process adapter
  running as the service user can act as one through the socket. Threat
  model B2 accepts this (C-API-1).
- There is no credential database to leak, back up or rotate, and audit
  records name callers by stable identity provider IDs (C-AUDIT-6).
- Roles are only as good as the identity provider's groups. Changing
  who is an administrator is done there, not in Bearing.
- The `Authorizer` interface and its conformance suite are in
  `pkg/contracts`, with the default group-to-role backend in `pkg/auth`. The
  token verification and the API servers arrive with M3 (durable, always-on
  core), and the status above flips to accepted in the change that makes the
  server enforce them.
