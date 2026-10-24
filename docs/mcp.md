# MCP: Bearing's answers for agents

`bearing mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server on stdin and stdout. An agent runs it as a command and asks the same
questions as the `bearing` queries, getting the same answers with their
sources, observed times and confidence.

It reads from a running server (`--server`, a Unix socket path or
`https://host:port`, with the bearer token in the variable `--token-env`
names, default `$BEARING_TOKEN`) or straight from a store (`--store`).
`$BEARING_SERVER` and `$BEARING_STORE` stand in for the flags.

`--store` opens the DuckDB file directly. It is for a stopped server or a
demo; beside a running server, use `--server`.

## Add it to an agent

```sh
# Claude Code
claude mcp add bearing -- bearing mcp --server /run/bearing/api.sock

# a server on the network: name the variable the agent host already sets
claude mcp add bearing -- bearing mcp --server https://bearing.example:8443 --token-env BEARING_AGENT_TOKEN
```

The token comes from the agent host's environment. Never write its value in
the client's configuration (no `-e BEARING_TOKEN=<value>`): that file is
read by other tools and often committed or synced.

Claude Desktop takes the same command in `claude_desktop_config.json`:

```json
{"mcpServers": {"bearing": {"command": "bearing", "args": ["mcp", "--server", "/run/bearing/api.sock"]}}}
```

## Tools

All tools are read-only. There is no tool to accept or reject an identity
link or to request a sync: only a person decides links
([ADR 7](adr/0007-records-identity-links-matchers.md)), with `bearing link`.

| Tool | Asks |
| --- | --- |
| `find_entities(text, kind?, limit?)` | Which entity has this name? |
| `get_entity(ref, as_of?)` | What is asserted about it? |
| `owners(ref, as_of?)` | Who owns it? |
| `changes(ref?, since, until?, limit?)` | What changed in a window? |
| `depends_on(ref, depth?, as_of?)` | What does it depend on, and how? |
| `depended_on_by(ref, depth?, as_of?)` | What depends on it, and how? |
| `review_queue(limit?)` | Which identity links wait for a person? |

A ref is an entity ID or a key such as `github:repo/acme/payments-api`; times
are RFC 3339, a date, or a duration back from now (`72h`, `7d`).

An answer is the query layer's JSON. In `content[0].text` it is the JSON
text itself, so every protocol version (2024-11-05, 2025-03-26 and
2025-06-18) gets the data; `content[1].text` is a one-line count. Clients
that negotiate 2025-06-18 also get it as `structuredContent`, and each tool
lists an `outputSchema` of the answer's top-level fields. A failure is a
tool result with `isError` and the API's code first (`not_found`,
`invalid_request`, `timeout`, `unauthenticated`), with one fixed message per
code, the same from a store and from a server. An unknown tool or method,
a bad request id or an unknown `tools/list` cursor is a JSON-RPC error.

Limits: a request line is at most 1 MiB (a longer one is refused and
skipped), arguments are decoded strictly (an unknown field or an explicit
`limit` or `depth` of 0 is refused), `find_entities` text is at most 256
bytes, and each call has the server's request deadline (shorter for the slow
reads). JSON-RPC batches are answered only under 2025-03-26, the one version
that has them.

## Who is asking

**MCP calls are not marked as agent calls.** The server cannot tell an agent
from the person who started it: the caller is whoever launched `bearing mcp`,
with that user's rights, as with any `bearing` command. Only the absence of
write tools keeps an agent from deciding identity links. So:

- Never run the agent as the service user on the API socket: in the default
  `api.link_decisions: socket` mode that user may decide links, and the agent
  can run `bearing link` itself.
- Never turn on `api.link_decisions: token` for a token an agent holds.
- Give the agent its own token (or a Unix account that is not the service
  user), read from its host's environment.

See C-MCP-1 and the "agents are not marked" gap in the
[threat model](security/threat-model.md).

Names, attributes, qualifiers and link reasons come from people and source
systems. The tool descriptions say to treat them as data, and the count line
never holds them.

Each call is a span named `mcp.tool <name>` ([telemetry](telemetry.md)).
