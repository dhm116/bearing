# CLAUDE.md

@AGENTS.md

## Claude Code notes

- `AGENTS.md` (imported above) is the single source of project guidance.
  Put rules there, not here, so every agent sees them.
- Repo skills live in `.claude/skills/`: `new-adapter` and `new-backend`.
- `.claude/settings.json` pre-approves the read-only and build commands this
  repo uses (`make`, `go test`, `go vet`, `go build`, `gofmt`).
