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

Use a server on TCP with a token. The token comes from the agent host's
environment, named by `--token-env`; never write its value in the client's
configuration (no `-e BEARING_AGENT_TOKEN=<value>`): that file is read by
other tools and often committed or synced.

```sh
# Claude Code
claude mcp add bearing -- bearing mcp --server https://bearing.example:8443 --token-env BEARING_AGENT_TOKEN
```

Claude Desktop takes the same command in `claude_desktop_config.json`:

```json
{"mcpServers": {"bearing": {"command": "bearing", "args": ["mcp", "--server", "https://bearing.example:8443", "--token-env", "BEARING_AGENT_TOKEN"]}}}
```

A socket works too (`--server /run/bearing/api.sock`), but only when
`api.link_decisions: off`: the socket serves only the service user and root,
and with link decisions on, both may decide links, so an agent running as
either could run `bearing link` itself.

Never point an agent's `--store` at the live store file: whoever can write it
can change links directly. Give an agent only a copy or a demo store.

## Tools

All tools are read-only. There is no tool to accept or reject an identity
link or to request a sync: only a person decides links
([ADR 7](adr/0007-records-identity-links-matchers.md)), with `bearing link`.

| Tool | Asks |
| --- | --- |
| `find_entities(text, kind?, limit?)` | Which entity has this name? |
| `get_entity(ref, as_of?)` | What is asserted about it? |
| `owners(ref, as_of?, members?)` | Who owns it, and who are the people on the owning team (role, since when)? `members` defaults to true; `false` omits the people |
| `changes(ref?, since, until?, limit?)` | What changed in a window? |
| `depends_on(ref, depth?, as_of?)` | What does it depend on, and how? |
| `depended_on_by(ref, depth?, as_of?)` | What depends on it, and how? |
| `runtimes(language, below?, kind?, limit?, as_of?)` | Which services declare a language version below this one? |
| `review_queue(limit?)` | Which identity links wait for a person? |

A ref is an entity ID or a key such as `github:repo/acme/payments-api`; times
are RFC 3339, a date, or a duration back from now (`72h`, `7d`).

An answer is the query layer's JSON. In `content[0].text` it is the JSON
text itself, so every protocol version (2024-11-05, 2025-03-26 and
2025-06-18) gets the data; `content[1].text` is a one-line count. Clients
that negotiate 2025-06-18 also get it as `structuredContent`, and each tool
lists an `outputSchema` of the answer's top-level fields. A failure is a
tool result with `isError` and the API's code first (`not_found`,
`invalid_request`, `timeout`, `unauthenticated`), the same from a store and
from a server. The message is fixed for `not_found`, `timeout` and
`internal_error`; for `invalid_request` it is the query layer's reason; for
other codes it is the server's message and hint. An unknown tool or method,
a bad request id or an unknown `tools/list` cursor is a JSON-RPC error.

Limits: a batch is at most 32 messages and may not contain `initialize`; a request line is at most 1 MiB (a longer one is refused and
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

- Never run the agent as the service user or root on the API socket unless
  `api.link_decisions: off`: otherwise it can run `bearing link` itself.
- Never turn on `api.link_decisions: token` for a token an agent holds.
- Give the agent its own token, read from its host's environment.
- Never point its `--store` at the live store file.

See C-MCP-1 and the "agents are not marked" gap in the
[threat model](security/threat-model.md).

Names, attributes, qualifiers and link reasons come from people and source
systems. The tool descriptions say to treat them as data, and the count line
never holds them.

Every tool carries `readOnlyHint` and `openWorldHint: false` annotations (not under 2024-11-05, which has none).

Each call is a span named `mcp.tool <name>` ([telemetry](telemetry.md)).
