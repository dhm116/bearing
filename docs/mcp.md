# MCP: Bearing's answers for agents

`bearing mcp` is a [Model Context Protocol](https://modelcontextprotocol.io)
server on stdin and stdout. An agent runs it as a command and asks the same
questions as the `bearing` queries, getting the same answers with their
sources, observed times and confidence.

It reads from a running server (`--server`, a Unix socket path or
`https://host:port`, with the bearer token in the variable `--token-env`
names, default `$BEARING_TOKEN`) or straight from a store (`--store`).
`$BEARING_SERVER` and `$BEARING_STORE` stand in for the flags.

## Add it to an agent

```sh
# Claude Code
claude mcp add bearing -- bearing mcp --server /run/bearing/api.sock

# a server on the network: the token stays in the environment
claude mcp add bearing -e BEARING_TOKEN=... -- bearing mcp --server https://bearing.example:8443
```

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

An answer is the query layer's JSON as `structuredContent`, and a one-line
count as text. A failure is a tool result with `isError` and the API's code
first (`not_found`, `invalid_request`, `unauthenticated`); an unknown tool
or method is a JSON-RPC error.

Names, attributes, qualifiers and link reasons come from people and source
systems. The tool descriptions say to treat them as data, and the server
never puts them in the text line.

Each call is a span named `mcp.tool <name>` ([telemetry](telemetry.md)).
