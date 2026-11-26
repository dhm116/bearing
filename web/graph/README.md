# Bearing graph (spike)

A live picture of what Bearing knows: every entity as a card, the facts
between them as lines, and identity links Bearing is unsure of as dotted
lines. It updates while the demo story plays.

Spike only (`spike/graph-svelte`), never merged: `main` holds Go and shell
only.

Built with SvelteKit 3 on Bun, daisyUI 5 (Bearing's brand colours as the
`bearing-dark` and `bearing-light` themes), Svelte Flow 2 for the canvas and
ELK for the layout.

## Run it

You need Docker running and [Bun](https://bun.sh). From the repository root:

| Command | What it does |
| --- | --- |
| `make demo-start` | Starts Bearing and the Acme fakes ([docs/demo.md](../../docs/demo.md)) and this app in the background |
| `make demo-stop` | Stops all three; the data stays |
| `make demo-reset` | Stops all three and deletes the data, so the story starts over |
| `make demo-status` | Shows what is running |

`demo/demo.sh` does the work: it copies `.env.example` to `.env` if needed,
fetches the demo CA, and runs the app on port 5180 (`DEMO_UI_PORT` changes
it), logging to `web/graph/.dev.log`.

Open http://localhost:5180. **Sync now** reads the three Acme sources;
**Next step** plays one change of the story and syncs; **Auto** plays one
every six seconds.

## How it works

```
browser ──EventSource──▶ /api/stream ──poll 1.5 s──▶ Bearing GET /v1/graph
        ──fetch────────▶ /api/demo   ──────────────▶ fakes /demo/advance, Bearing sync
```

- `GET /v1/graph` (spike route in `pkg/server`) returns `pkg/graphview`'s
  picture, the one `bearing graph` draws.
- The SvelteKit server holds the API token and polls; the browser gets the
  picture over server-sent events only when it changes.
- The page diffs each new picture against the last for the "What changed"
  feed and to glow what is new (green) or renamed (amber).
- Columns follow kinds: people, teams, repositories, components,
  environments. Click a card to focus it and its neighbours and read its
  records, attributes, facts with their evidence, and identity links.

| Variable | Default |
| --- | --- |
| `BEARING_SERVER` | `https://localhost:8443` |
| `BEARING_TOKEN` | none: required |
| `BEARING_CA` | none (system roots) |
| `BEARING_DEMO_CONTROL` | `http://127.0.0.1:9090` |
| `BEARING_SOURCES` | `github-acme,directory,kubernetes-acme` |
