# tools

A separate Go module for developer tools, so the root `go.mod` stays as it
is. The Makefile builds them into `bin/tools/`.

| Tool | Version | Licence |
| --- | --- | --- |
| [golangci-lint](https://github.com/golangci/golangci-lint) | v2.14.0 | GPL-3.0 |
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | v1.8.0 | BSD-3-Clause |
| `covergate` (this directory) | | Apache-2.0, standard library only |

These are development and CI tools only. Bearing code never imports them,
they are never linked into a Bearing binary, and they are not distributed
with Bearing's artifacts, so golangci-lint's GPL-3.0 licence does not reach
Bearing (AGENTS.md, architecture rule 6). A tool added here needs an
OSI-approved licence and a row in this table.

Update a pin with `go -C tools get -tool <module>@<version>`.
