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

If the **Report a vulnerability** button is missing, contact the maintainer
through the contact listed on their GitHub profile
([@dhm116](https://github.com/dhm116)) and ask for a private channel; do
not include details.

Include what you can:

- the affected component and commit
- steps to reproduce, or a proof of concept
- the impact you expect (what an attacker gains)

## What to expect

- The maintainer acknowledges the report within 5 working days.
- The maintainer confirms or rules out the issue and shares an assessment
  within 14 days of acknowledging it.
- The default disclosure date is when a fix is on `main`. If there is no
  fix after 90 days, the maintainer agrees a date with you.
- Confirmed issues get a GitHub security advisory, crediting you unless you
  ask otherwise.

These are targets, not guarantees. If you have heard nothing after 5
working days, add a comment to your advisory or follow up on the private
channel.

## Scope

In scope: code in this repository, the release artifacts and container
images built from it, and the deployment files it ships (for example the
compose file).

Out of scope: vulnerabilities in source systems, identity providers or
DuckDB itself (report those upstream). Risks that an `insecure_*` setting knowingly
accepts are out of scope; a setting that fails to warn is in scope.

## Threat model

The trust boundaries, controls and known gaps are in
[`docs/security/threat-model.md`](docs/security/threat-model.md). It is a
short model for the code as it is now, written after Bearing restarted from
a smaller scope. The earlier model is at the
[`pre-pivot`](https://github.com/dhm116/bearing/blob/pre-pivot/docs/security/threat-model.md) tag.
