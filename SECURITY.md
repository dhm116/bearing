# Security policy

## Supported versions

Bearing is pre-1.0. Only the `main` branch is supported. Fixes land on
`main`; there are no backports to earlier commits or tags.

## Reporting a vulnerability

Please report vulnerabilities privately. Do not open a public issue, pull
request or discussion.

Use GitHub private vulnerability reporting: go to the
[Security tab of dhm116/bearing](https://github.com/dhm116/bearing/security)
and choose **Report a vulnerability**, or open
[a new draft advisory](https://github.com/dhm116/bearing/security/advisories/new)
directly.

Include what you can:

- the affected component and commit
- steps to reproduce, or a proof of concept
- the impact you expect (what an attacker gains)
- any threat or control IDs from the [threat model](docs/security/threat-model.md)
  that the issue breaks, for example `C-ADAPTER-5`

## What to expect

- We acknowledge the report within 5 working days.
- We confirm or rule out the issue and tell you our assessment within
  14 days of acknowledging it.
- We agree a disclosure date with you. The default is when a fix is on
  `main`, and no later than 90 days after the report.
- We publish a GitHub security advisory for confirmed issues and credit you
  unless you ask us not to.

Bearing is maintained by volunteers, so these are targets, not guarantees.
If you have heard nothing after 5 working days, add a comment to your
advisory.

## Scope

In scope: code in this repository, the release artifacts and container
images built from it, and the deployment files it ships (for example the
compose file).

Out of scope: vulnerabilities in source systems, identity providers or
SurrealDB themselves (report those upstream), and the risks the threat model
lists as accepted.

## Threat model

[docs/security/threat-model.md](docs/security/threat-model.md) describes
Bearing's trust boundaries, the threats at each one and the controls that
answer them.
